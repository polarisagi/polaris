package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
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

	// 批量删除
	for _, key := range keysToDelete {
		_ = ca.store.Delete(ctx, key)
	}

	// 对支持 SQL 的引擎触发 VACUUM——通过 Txn 内的 Raw SQL 能力
	if sqlStore, ok := ca.store.(protocol.SQLQuerier); ok {
		// SQLite 引擎可通过额外接口执行
		_, _ = sqlStore.ExecContext(ctx, "PRAGMA incremental_vacuum(256)")
	}

	_ = deleted
	return nil
}
