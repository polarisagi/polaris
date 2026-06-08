package swarm

import (
	"context"
	"sync"
	"time"
)

// Reaper — 孤儿任务回收器。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §1.7

type Reaper struct {
	blackboard   *SQLiteBlackboard
	scanInterval time.Duration // 1s
	gcInterval   time.Duration // 30s
	mu           sync.Mutex
}

func NewReaper(bb *SQLiteBlackboard) *Reaper {
	return &Reaper{
		blackboard:   bb,
		scanInterval: 1 * time.Second,
		gcInterval:   30 * time.Second,
	}
}

// Phase1 扫描过期租约。
// 调用底层 Blackboard 触发并发 cancel() 与 5s 宽限期，随后转为 Pending 状态并更新 Version 防 TOCTOU。
func (r *Reaper) Phase1(ctx context.Context) {
	r.blackboard.reap(ctx)
}

// Phase2 驱逐终态任务。
// Status∈{Done,Failed} + UpdatedAt+5min<now → 删除。
func (r *Reaper) Phase2(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, _ = r.blackboard.db.ExecContext(ctx, `
		DELETE FROM tasks
		WHERE status IN ('done', 'failed') AND updated_at < datetime('now', '-5 minute')
	`)
}

func (r *Reaper) Run(ctx context.Context) {
	tickerScan := time.NewTicker(r.scanInterval)
	tickerGC := time.NewTicker(r.gcInterval)
	defer tickerScan.Stop()
	defer tickerGC.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tickerScan.C:
			r.Phase1(ctx)
		case <-tickerGC.C:
			r.Phase2(ctx)
		}
	}
}

// SupervisorEpoch 启动时 [Storage-SQLite] sys_config 原子递增 orchestrator_epoch。
// Worker 拉取式: SideEffectPreCheck 时读 epoch (O(1), <0.1ms)。
// 不一致 → GracefulTermination + 重注册。
type SupervisorEpoch struct {
	epoch int64
}

// Get 返回当前 epoch。
func (se *SupervisorEpoch) Get() int64 {
	return se.epoch
}

// Increment 原子递增 epoch。
func (se *SupervisorEpoch) Increment() {
	se.epoch++
}
