package orchestrator

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

// 2026-07-14（ADR-0051）：SupervisorEpoch 删除——ADR-0050 淘汰的中心化
// Orchestrator "Worker 拉取式读 epoch 校验"设计的孤儿残留，全仓零调用点。
// 当前并发控制走 claimed_by/claimed_at/expires_at 租约认领模型
// （sqlite_blackboard_reaper.go），与本类型描述的机制无关。
