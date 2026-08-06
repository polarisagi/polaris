package consolidation

import (
	"math"

	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func NewForgettingManager(store protocol.Store, cognitive protocol.CognitiveSearcher, decayRate float64) *ForgettingManager {
	return &ForgettingManager{
		store:             store,
		cognitive:         cognitive,
		decayRate:         decayRate,
		salienceThreshold: 0.15,

		archiver: NewColdArchiver(store),
	}
}

// UpdateDecay 更新衰减权重（纯时间衰减）。
// ageHours = now - timestamp; DecayWeight = salience × exp(-decayRate × ageHours/24).
//
// 保留此签名供无检索统计的降级路径（纯 KV 部署）使用；SQL 路径一律走
// UpdateDecayReinforced，见其注释说明为什么纯时间衰减不够。
func (fm *ForgettingManager) UpdateDecay(salience float64, ageHours float64) float64 {
	decay := salience * math.Exp(-fm.decayRate*ageHours/24.0)
	return decay
}

// reinforcementCap 检索强化的权重上限倍数。
// 封顶而非无限放大：否则一条被反复命中的记忆会获得近乎永久的豁免，
// 遗忘机制对高频区失效，退化成"只清理冷数据"。
const reinforcementCap = 3.0

// idleDecayPenaltyHours 超过此时长未被检索则开始施加闲置惩罚。
// 取 14 天：短于典型的"隔周回到同一个项目"周期会误伤周期性使用的记忆。
const idleDecayPenaltyHours = 14 * 24

// UpdateDecayReinforced 在时间衰减基础上叠加检索强化与闲置惩罚（GD-14-003）。
//
//	weight = salience × exp(-rate·age/24) × reinforce(count) × idlePenalty(idleHours)
//
// 为什么纯时间衰减不够：它表达的是"越老越该忘"，会误删**旧但持续有用**的
// 记忆（项目约定、用户长期偏好、反复踩过的坑），同时留下大量"新但从没人
// 用过"的噪声——与 GD-14-003 要提升的检索信噪比恰好相反。真正该淘汰的是
// "没人用的"，不是"旧的"。
//
//   - reinforce：命中次数的对数增益（前几次命中收益大，之后边际递减），
//     封顶 reinforcementCap，避免高频记忆获得永久豁免。
//   - idlePenalty：从未被检索、或超过 idleDecayPenaltyHours 未命中的条目
//     额外加速衰减。这是"激进遗忘"真正的着力点——它专打噪声，不误伤热数据。
//
// retrievalCount<=0 且 lastRetrievedAtMs<=0 表示从未被检索过。
func (fm *ForgettingManager) UpdateDecayReinforced(
	salience, ageHours float64, retrievalCount int, lastRetrievedAtMs int64,
) float64 {
	weight := fm.UpdateDecay(salience, ageHours)

	if retrievalCount > 0 {
		// 1 次命中 ≈ ×1.69，5 次 ≈ ×2.79，10 次封顶 ×3.0
		boost := 1.0 + math.Log1p(float64(retrievalCount))
		weight *= math.Min(boost, reinforcementCap)
	}

	idleHours := ageHours // 从未被检索：闲置时长等同于存在时长
	if lastRetrievedAtMs > 0 {
		idleHours = float64(time.Now().UnixMilli()-lastRetrievedAtMs) / 3600000.0
	}
	if idleHours > idleDecayPenaltyHours {
		// 超期闲置按额外一轮指数衰减惩罚，闲置越久惩罚越重。
		weight *= math.Exp(-fm.decayRate * (idleHours - idleDecayPenaltyHours) / 24.0)
	}
	return weight
}

// PeriodicCleanup 扫描 Episodic 事件，将低于 salienceThreshold 的条目标记为可遗忘，
// 超过 30 天且低 salience 的条目移入冷归档。
// 不物理删除——仅写入 tombstone 标记，由 ColdArchiver.PhysicalCompact 负责最终清理。
func (fm *ForgettingManager) PeriodicCleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// P1: Optimization - Use SQL native query if possible
	if sqlStore, ok := fm.store.(protocol.SQLQuerier); ok {
		if err := fm.cleanupWithSQL(ctx, sqlStore); err == nil {
			return nil
		}
	}

	return fm.cleanupWithKV(ctx)
}

// decayUpdateItem 是 cleanupWithSQL 分流出的"需更新 decay_weight"条目。
type decayUpdateItem struct {
	ID          int64
	DecayWeight float64
}

// archiveItem 是 cleanupWithSQL 分流出的"需归档"条目。
type archiveItem struct {
	ID        int64
	EventUUID string
}

// txBeginner 是可选的事务开启能力（db 未实现时降级为非事务单语句执行）。
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func (fm *ForgettingManager) cleanupWithSQL(ctx context.Context, db protocol.SQLQuerier) error {
	now := time.Now().UnixMilli()
	toUpdate, toArchive, err := fm.queryDecayCandidates(ctx, db, now)
	if err != nil {
		return err
	}

	txDB, _ := db.(txBeginner)

	fm.applyDecayUpdates(ctx, db, txDB, toUpdate)
	fm.applyArchival(ctx, db, txDB, toArchive, now)

	return nil
}

// queryDecayCandidates 扫描未归档且 salience<1.0 的 episodic 事件，按衰减权重分流为
// "需要更新 decay_weight" 与 "需要归档"（从 cleanupWithSQL 拆出，gocyclo 治理，行为不变）。
func (fm *ForgettingManager) queryDecayCandidates(ctx context.Context, db protocol.SQLQuerier, now int64) ([]decayUpdateItem, []archiveItem, error) {
	// GD-14-003：一并取出检索强化信号（retrieval_count / last_retrieved_at），
	// 让淘汰依据从"够不够旧"变为"有没有人用"。
	rows, err := db.QueryContext(ctx, `
		SELECT id, salience, occurred_at, event_uuid,
		       retrieval_count, COALESCE(last_retrieved_at, 0)
		FROM episodic_events
		WHERE archived = 0 AND salience < 1.0`)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "ForgettingManager.cleanupWithSQL", err)
	}
	defer rows.Close()

	var toUpdate []decayUpdateItem
	var toArchive []archiveItem

	for rows.Next() {
		var id int64
		var salience float64
		var occurredAt int64
		var eventUUID string
		var retrievalCount int
		var lastRetrievedAt int64
		if err := rows.Scan(&id, &salience, &occurredAt, &eventUUID, &retrievalCount, &lastRetrievedAt); err != nil {
			continue
		}

		ageHours := float64(now-occurredAt) / 3600000.0
		decayWeight := fm.UpdateDecayReinforced(salience, ageHours, retrievalCount, lastRetrievedAt)

		if decayWeight >= fm.salienceThreshold {
			continue
		}
		if ageHours > 30*24 {
			toArchive = append(toArchive, archiveItem{ID: id, EventUUID: eventUUID})
		} else {
			toUpdate = append(toUpdate, decayUpdateItem{ID: id, DecayWeight: decayWeight})
		}
	}
	return toUpdate, toArchive, nil
}

// applyDecayUpdates 批量写入 decay_weight 更新，事务可用时同步写 change_log
// （从 cleanupWithSQL 拆出，gocyclo 治理，行为不变）。
func (fm *ForgettingManager) applyDecayUpdates(ctx context.Context, db protocol.SQLQuerier, txDB txBeginner, toUpdate []decayUpdateItem) {
	for _, item := range toUpdate {
		if txDB == nil {
			_, err := db.ExecContext(ctx, "UPDATE episodic_events SET decay_weight=? WHERE id=?", item.DecayWeight, item.ID)
			if err != nil {
				slog.Warn("ForgettingManager.cleanupWithSQL: update decay_weight failed", "id", item.ID, "err", err)
			}
			continue
		}

		tx, err := txDB.BeginTx(ctx, nil)
		if err != nil {
			slog.Warn("ForgettingManager.cleanupWithSQL: begin tx failed", "id", item.ID, "err", err)
			continue
		}

		_, err = tx.ExecContext(ctx, "UPDATE episodic_events SET decay_weight=? WHERE id=?", item.DecayWeight, item.ID)
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO episodic_events_change_log(event_id, operation, payload, occurred_at) VALUES (?, 'UPDATE', ?, ?)", item.ID, fmt.Sprintf(`{"decay_weight":%f}`, item.DecayWeight), time.Now().UnixMilli())
		}
		if err != nil {
			_ = tx.Rollback() //nolint:errcheck // 回滚失败无补救手段，错误来源已在下方日志中
			slog.WarnContext(ctx, "ForgettingManager.cleanupWithSQL: update decay_weight tx failed", "id", item.ID, "err", err)
			continue
		}
		if cErr := tx.Commit(); cErr != nil {
			// 衰减权重未落盘：本条下轮会以旧 decay_weight 重新计算，
			// 结果收敛（幂等），故不重试；但持续失败意味着遗忘机制整体停摆。
			slog.WarnContext(ctx, "ForgettingManager: decay_weight commit failed, will recompute next cycle",
				"id", item.ID, "err", cErr)
		}
	}
}

// applyArchival 批量归档条目（archived=1），事务可用时同步写 change_log，并同步删除
// 认知索引 FTS/Vec 条目（从 cleanupWithSQL 拆出，gocyclo 治理，行为不变）。
func (fm *ForgettingManager) applyArchival(ctx context.Context, db protocol.SQLQuerier, txDB txBeginner, toArchive []archiveItem, now int64) {
	for _, item := range toArchive {
		if txDB == nil {
			// archived=1 + archive_offset 填充
			_, err := db.ExecContext(ctx, "UPDATE episodic_events SET archived=1, archive_offset=? WHERE id=?", now, item.ID)
			if err != nil {
				slog.Warn("ForgettingManager.cleanupWithSQL: archive failed", "id", item.ID, "err", err)
			}
			fm.deleteCognitiveIndex(item.EventUUID)
			continue
		}

		tx, err := txDB.BeginTx(ctx, nil)
		if err != nil {
			slog.Warn("ForgettingManager.cleanupWithSQL: begin tx failed for archive", "id", item.ID, "err", err)
			continue
		}

		_, err = tx.ExecContext(ctx, "UPDATE episodic_events SET archived=1, archive_offset=? WHERE id=?", now, item.ID)
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO episodic_events_change_log(event_id, operation, payload, occurred_at) VALUES (?, 'ARCHIVE', '{}', ?)", item.ID, time.Now().UnixMilli())
		}

		if err != nil {
			_ = tx.Rollback() //nolint:errcheck // 回滚失败无补救手段，错误来源已在下方日志中
			slog.WarnContext(ctx, "ForgettingManager.cleanupWithSQL: archive tx failed", "id", item.ID, "err", err)
			continue
		}
		// Commit 成功才删认知索引——顺序不可交换，且 Commit 失败必须跳过删除。
		// 此前是 `_ = tx.Commit()` 后无条件 deleteCognitiveIndex：若 Commit 失败，
		// 行仍是 archived=0（可检索状态），但它的 FTS/Vec 索引条目已被删掉，
		// 该条记忆就此对检索**不可见**却仍占存储，且 DB 里看不出任何异常。
		if cErr := tx.Commit(); cErr != nil {
			slog.WarnContext(ctx, "ForgettingManager: archive commit failed, keeping cognitive index intact",
				"id", item.ID, "err", cErr)
			continue
		}
		fm.deleteCognitiveIndex(item.EventUUID)
	}
}

// deleteCognitiveIndex 同步删除认知索引 FTS/Vec 条目（从 cleanupWithSQL 拆出，
// gocyclo 治理，行为不变）。
func (fm *ForgettingManager) deleteCognitiveIndex(eventUUID string) {
	if fm.cognitive == nil || eventUUID == "" {
		return
	}
	_ = fm.cognitive.FTSDelete("ep_" + eventUUID)
	_ = fm.cognitive.VecDelete("ep_" + eventUUID)
}

func (fm *ForgettingManager) cleanupWithKV(ctx context.Context) error {
	iter, err := fm.store.Scan(ctx, []byte("events:"))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PeriodicCleanup: scan events 失败", err)
	}
	defer iter.Close()

	for iter.Next() {
		key := iter.Key()
		val := iter.Value()

		var ev struct {
			ID         string  `json:"id"`
			Topic      string  `json:"topic"`
			Salience   float64 `json:"salience"`
			OccurredAt int64   `json:"occurred_at"`
		}
		if err := json.Unmarshal(val, &ev); err != nil {
			continue
		}

		if ev.Topic != "memory.openclaw" && ev.Topic != "memory" {
			continue
		}

		ageHours := float64(time.Now().UnixMilli()-ev.OccurredAt) / 3600000.0
		decayWeight := fm.UpdateDecay(ev.Salience, ageHours)

		if decayWeight < fm.salienceThreshold {
			fm.processForgettableItemKV(ctx, ev.ID, decayWeight, ageHours, key, val)
		}
	}

	if iter.Err() != nil {
		return apperr.Wrap(apperr.CodeInternal, "PeriodicCleanup: 迭代失败", iter.Err())
	}
	return nil
}

// processForgettableItemKV 为一条低价值记忆打 tombstone；超过 30 天的再做冷归档。
//
// 三处 store 调用此前均为 `_ =` 静默丢弃（2026-08-06 修复），每一处失败都有
// 明确且不可自愈的后果，必须可观测（HE-1 / HE-6）：
//   - tombstone 写失败 → ColdArchiver.PhysicalCompact 永远扫不到这条，
//     该记忆事实上"永不被遗忘"，遗忘机制对它静默失效；
//   - 归档 Put 失败后若仍继续删热存储 → 记忆直接永久丢失（无处可恢复），
//     因此这里改为**归档失败即中止本条处理**，宁可留在热存储等下一轮；
//   - 热存储 Delete 失败 → 同一条记忆同时存在于热存储与 archive: 前缀，
//     后续检索会重复命中，且下一轮会重复归档。
func (fm *ForgettingManager) processForgettableItemKV(ctx context.Context, id string, decayWeight float64, ageHours float64, key, val []byte) {
	tombstoneKey := fmt.Appendf(nil, "forgettable:%s", id)
	tombstoneVal := fmt.Appendf(nil, `{"id":"%s","decay_weight":%.4f,"marked_at":%d}`, id, decayWeight, time.Now().UnixMilli())
	if err := fm.store.Put(ctx, tombstoneKey, tombstoneVal); err != nil {
		slog.ErrorContext(ctx, "forgetting: tombstone write failed, item will never be reclaimed",
			"id", id, "decay_weight", decayWeight, "err", err)
		return
	}

	if ageHours <= 30*24 {
		return
	}

	// 冷归档：先确保归档副本落盘，再删热存储——顺序不可颠倒。
	archiveKey := fmt.Appendf(nil, "archive:episodic:%s", id)
	if err := fm.store.Put(ctx, archiveKey, val); err != nil {
		slog.ErrorContext(ctx, "forgetting: cold archive write failed, keeping hot copy (will retry next cycle)",
			"id", id, "err", err)
		return
	}
	if err := fm.store.Delete(ctx, key); err != nil {
		slog.ErrorContext(ctx, "forgetting: hot copy delete failed, memory now duplicated in hot store and archive",
			"id", id, "err", err)
		return
	}
	if err := fm.store.Delete(ctx, tombstoneKey); err != nil {
		// 归档已完成，仅残留一个 tombstone：PhysicalCompact 下一轮会尝试删除
		// 一个已不存在的 key，无实质损害，故只 Warn 不中止。
		slog.WarnContext(ctx, "forgetting: tombstone cleanup failed, stale marker left behind",
			"id", id, "err", err)
	}
}

// ColdArchiver 冷归档器。
// 将超期低价值事件从热存储移到归档前缀，SQLite 物理 VACUUM 回收磁盘。
// store 通过协议抽象访问持久化层。
type ColdArchiver struct {
	store         protocol.Store
	archivePath   string // ~/.polarisagi/polaris/archive/
	retentionDays int    // 热库 30d, 冷库无限
}
