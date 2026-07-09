package consolidation

import (
	"math"

	"context"
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
		qLearner:          NewQLearner(0.1, 0.9),
		archiver:          NewColdArchiver(store),
	}
}

// UpdateDecay 更新衰减权重。
// ageHours = now - timestamp; DecayWeight = salience × exp(-decayRate × ageHours/24).
func (fm *ForgettingManager) UpdateDecay(salience float64, ageHours float64) float64 {
	decay := salience * math.Exp(-fm.decayRate*ageHours/24.0)
	return decay
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

func (fm *ForgettingManager) cleanupWithSQL(ctx context.Context, db protocol.SQLQuerier) error {
	rows, err := db.QueryContext(ctx, "SELECT id, salience, occurred_at, event_uuid FROM episodic_events WHERE archived = 0 AND salience < 1.0")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ForgettingManager.cleanupWithSQL", err)
	}
	defer rows.Close()

	var toUpdate []struct {
		ID          int64
		DecayWeight float64
	}
	var toArchive []struct {
		ID        int64
		EventUUID string
	}

	now := time.Now().UnixMilli()
	for rows.Next() {
		var id int64
		var salience float64
		var occurredAt int64
		var eventUUID string
		if err := rows.Scan(&id, &salience, &occurredAt, &eventUUID); err != nil {
			continue
		}

		ageHours := float64(now-occurredAt) / 3600000.0
		decayWeight := fm.UpdateDecay(salience, ageHours)

		if decayWeight < fm.salienceThreshold {
			if ageHours > 30*24 {
				toArchive = append(toArchive, struct {
					ID        int64
					EventUUID string
				}{id, eventUUID})
			} else {
				toUpdate = append(toUpdate, struct {
					ID          int64
					DecayWeight float64
				}{id, decayWeight})
			}
		}
	}
	rows.Close()

	for _, item := range toUpdate {
		_, err := db.ExecContext(ctx, "UPDATE episodic_events SET decay_weight=? WHERE id=?", item.DecayWeight, item.ID)
		if err != nil {
			slog.Warn("ForgettingManager.cleanupWithSQL: update decay_weight failed", "id", item.ID, "err", err)
		}
	}

	for _, item := range toArchive {
		// archived=1 + archive_offset 填充
		_, err := db.ExecContext(ctx, "UPDATE episodic_events SET archived=1, archive_offset=? WHERE id=?", now, item.ID)
		if err != nil {
			slog.Warn("ForgettingManager.cleanupWithSQL: archive failed", "id", item.ID, "err", err)
		}
		// 同步 cognitive.FTSDelete/VecDelete
		if fm.cognitive != nil && item.EventUUID != "" {
			_ = fm.cognitive.FTSDelete("ep_" + item.EventUUID)
			_ = fm.cognitive.VecDelete("ep_" + item.EventUUID)
		}
	}

	return nil
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

func (fm *ForgettingManager) processForgettableItemKV(ctx context.Context, id string, decayWeight float64, ageHours float64, key, val []byte) {
	tombstoneKey := fmt.Appendf(nil, "forgettable:%s", id)
	tombstoneVal := fmt.Appendf(nil, `{"id":"%s","decay_weight":%.4f,"marked_at":%d}`, id, decayWeight, time.Now().UnixMilli())
	_ = fm.store.Put(ctx, tombstoneKey, tombstoneVal)

	if ageHours > 30*24 {
		archiveKey := fmt.Appendf(nil, "archive:episodic:%s", id)
		_ = fm.store.Put(ctx, archiveKey, val)
		_ = fm.store.Delete(ctx, key)
		_ = fm.store.Delete(ctx, tombstoneKey)
	}
}

// QLearner Q-Learning 熵门控效用衰减。
// 用于自适应调整 salienceThreshold——高熵环境下更积极遗忘。
type QLearner struct {
	states map[string]float64
	alpha  float64 // 学习率
	gamma  float64 // 折扣因子
}

func NewQLearner(alpha, gamma float64) *QLearner {
	return &QLearner{
		states: make(map[string]float64),
		alpha:  alpha,
		gamma:  gamma,
	}
}

// Update 更新状态值。
func (ql *QLearner) Update(state string, reward float64) {
	ql.states[state] += ql.alpha * (reward - ql.states[state])
}

// ColdArchiver 冷归档器。
// 将超期低价值事件从热存储移到归档前缀，SQLite 物理 VACUUM 回收磁盘。
// store 通过协议抽象访问持久化层。
type ColdArchiver struct {
	store         protocol.Store
	archivePath   string // ~/.polarisagi/polaris/archive/
	retentionDays int    // 热库 30d, 冷库无限
}
