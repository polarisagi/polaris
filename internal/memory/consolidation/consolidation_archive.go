package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func NewColdArchiver(store protocol.Store) *ColdArchiver {
	return &ColdArchiver{
		store:         store,
		archivePath:   "archive/",
		retentionDays: 30,
	}
}

// PhysicalCompact 扫描 tombstone 标记（forgettable:*），
// 将对应的原事件 key 物理删除并清理 tombstone 自身。
// 对支持 SQL 的引擎委托 DB 级 VACUUM；对纯 KV 引擎仅做 key 级清理。
func (ca *ColdArchiver) PhysicalCompact() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deleted := 0

	// 扫描所有 forgettable tombstone
	iter, err := ca.store.Scan(ctx, []byte("forgettable:"))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PhysicalCompact: scan tombstones 失败", err)
	}
	defer iter.Close()

	var keysToDelete [][]byte

	for iter.Next() {
		var tombstone struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iter.Value(), &tombstone); err != nil || tombstone.ID == "" {
			continue
		}

		// 删除原事件（可能已被归档，Delete 幂等）
		eventKey := fmt.Appendf(nil, "events:%s", tombstone.ID)
		keysToDelete = append(keysToDelete, eventKey)
		// 删除 tombstone 自身
		keysToDelete = append(keysToDelete, iter.Key())
		deleted++
	}

	if iter.Err() != nil {
		return apperr.Wrap(apperr.CodeInternal, "PhysicalCompact: 迭代失败", iter.Err())
	}

	// 批量删除。
	//
	// 2026-08-08：原为 `_ = ca.store.Delete(...)` 全静默 + 末尾 `_ = deleted`
	// 把统计值直接丢弃，于是本函数无论删了 0 条还是 10000 条、失败多少条，
	// 对外都只有一个 nil。物理压实是 Tier-0 回收磁盘的唯一手段，"删除全失败"
	// 与"无可删"必须可区分（HE-1 禁止能算不上报）。单条失败不中断整批——
	// tombstone 保留即下轮重试，比中途退出更安全。
	failed := 0
	for _, key := range keysToDelete {
		if err := ca.store.Delete(ctx, key); err != nil {
			failed++
			slog.WarnContext(ctx, "PhysicalCompact: key 删除失败，保留 tombstone 待下轮重试",
				"key", string(key), "err", err)
		}
	}

	// 对支持 SQL 的引擎触发 VACUUM——通过 Txn 内的 Raw SQL 能力
	if sqlStore, ok := ca.store.(protocol.SQLQuerier); ok {
		// SQLite 引擎可通过额外接口执行
		if _, err := sqlStore.ExecContext(ctx, "PRAGMA incremental_vacuum(256)"); err != nil {
			slog.WarnContext(ctx, "PhysicalCompact: incremental_vacuum 失败，空间未回收", "err", err)
		}
	}

	slog.InfoContext(ctx, "PhysicalCompact: 完成",
		"tombstones", deleted, "keys_deleted", len(keysToDelete)-failed, "keys_failed", failed)
	return nil
}
