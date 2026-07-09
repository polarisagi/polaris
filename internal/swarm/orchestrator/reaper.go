package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"
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

// Phase2 驱逐终态任务并纠正 zombie/starvation 状态。
// 委托至 SQLiteBlackboard.reaperPhase2，确保调用 cancel() 后再改状态（避免 goroutine 泄漏）。
func (r *Reaper) Phase2(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blackboard.reaperPhase2(ctx)
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
	// epoch 必须 64 位对齐，使用 atomic 操作防止并发竞争（P1-3）。
	epoch int64
}

// Get 原子读取当前 epoch。
func (se *SupervisorEpoch) Get() int64 {
	return atomic.LoadInt64(&se.epoch)
}

// Increment 原子递增 epoch；并发安全（P1-3：原 se.epoch++ 为非原子操作）。
func (se *SupervisorEpoch) Increment() {
	atomic.AddInt64(&se.epoch, 1)
}
