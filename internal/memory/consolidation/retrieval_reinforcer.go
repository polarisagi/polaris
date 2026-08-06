package consolidation

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// 检索强化（GD-14-003）
//
// 遗忘机制此前只按 salience × exp(-rate·age) 衰减——纯粹是"越老越该忘"。
// 这会误删那些**旧但持续有用**的记忆（项目约定、用户长期偏好、反复踩过的坑），
// 同时留下大量"新但从没人用过"的噪声，恰好与 GD-14-003 想要的信噪比相反。
//
// 本文件补上缺失的另一半信号：记忆被检索命中即视为"有用"，获得抗遗忘强化。
// 这是认知科学里的间隔效应（spacing effect）在工程上的直接对应，也是
// 让"遗忘"从"按时间一刀切"变成"按实际价值淘汰"的关键。
//
// 性能约束（Tier-0）：**绝不在读路径上同步写库**。检索是高频只读操作，
// 每次命中都 UPDATE 会把只读路径变成写路径，在 2GB VPS 的单写者 SQLite 上
// 直接与主写入链路争锁。因此采用"内存累计 + 周期批量落盘"，
// 与 CorpusStats 的 dirty/FlushTo 模式同构。
// ============================================================================

// RetrievalReinforcer 累计检索命中并周期性批量落盘。
type RetrievalReinforcer struct {
	db protocol.SQLQuerier

	mu sync.Mutex
	// pending eventUUID → 本周期内的命中次数。
	pending map[string]int
}

// NewRetrievalReinforcer 创建强化器。db 为 nil 时所有方法退化为 no-op
// （纯 KV 部署无 episodic_events 表，检索强化不可用，但不应因此报错）。
func NewRetrievalReinforcer(db protocol.SQLQuerier) *RetrievalReinforcer {
	return &RetrievalReinforcer{db: db, pending: make(map[string]int)}
}

// Reinforce 记录一批被检索命中的记忆来源。
//
// sources 是 HybridRetriever 返回的 ScoredFragment.Source，形如
// "episodic:{event_uuid}"；非 episodic 前缀的来源（chunk:/reflection:/
// durative_group: 等）直接忽略——它们不在 episodic_events 表里。
//
// 只统计**真正进入最终结果**的片段，不统计召回阶段的中间候选：
// 被 RRF 融合淘汰掉的候选不代表"有用"，计入会让强化信号失真。
func (r *RetrievalReinforcer) Reinforce(sources []string) {
	if r == nil || r.db == nil || len(sources) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range sources {
		uuid, ok := strings.CutPrefix(s, "episodic:")
		if !ok || uuid == "" {
			continue
		}
		r.pending[uuid]++
	}
}

// Flush 把累计的命中批量写入 episodic_events。
//
// 单事务批量 UPDATE：命中集通常是几十条，逐条独立事务会在 SQLite 上产生
// 数十次 fsync。失败时**保留** pending（不清空），下一周期重试——
// 强化信号丢失会让本该保留的记忆被误遗忘，值得重试。
func (r *RetrievalReinforcer) Flush(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return nil
	}
	batch := r.pending
	r.pending = make(map[string]int)
	r.mu.Unlock()

	now := time.Now().UnixMilli()
	var failed error
	applied := 0
	for uuid, hits := range batch {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE episodic_events
			SET retrieval_count = retrieval_count + ?, last_retrieved_at = ?
			WHERE event_uuid = ?`, hits, now, uuid); err != nil {
			if failed == nil {
				failed = err
			}
			continue
		}
		applied++
	}

	if failed != nil {
		// 回滚未落盘的计数到 pending，等下一周期重试。
		r.mu.Lock()
		for uuid, hits := range batch {
			r.pending[uuid] += hits
		}
		r.mu.Unlock()
		return apperr.Wrap(apperr.CodeInternal, "retrieval_reinforcer: flush failed, counts retained for retry", failed)
	}
	slog.DebugContext(ctx, "retrieval_reinforcer: flushed", "events", applied)
	return nil
}

// PendingCount 返回尚未落盘的记忆条目数（观测/测试用）。
func (r *RetrievalReinforcer) PendingCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
