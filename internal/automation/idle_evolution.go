package automation

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// IdleEvolutionScheduler 在系统空闲期间主动触发记忆巴固、连弹和学习任务。
// 空闲判定： time.Since(lastActivityAt) > idleThreshold && rg.InFlight()==0
type IdleEvolutionScheduler struct {
	rg             *ResourceGovernor
	hw             *probe.HardwareProbe // 用于 Tier 门控
	idleThreshold  time.Duration
	lastActivityAt atomic.Int64 // Unix 纳秒，由 ResourceGovernor.Admit 更新
	// 可被注入的任务（Tier0 默认开启）
	consolidateFn func(ctx context.Context) error // consolidation.ConsolidationPipeline.Consolidate
	forgettingFn  func(ctx context.Context) error // ForgettingManager.PeriodicCleanup
}

func NewIdleEvolutionScheduler(rg *ResourceGovernor, hw *probe.HardwareProbe) *IdleEvolutionScheduler {
	s := &IdleEvolutionScheduler{
		rg:            rg,
		hw:            hw,
		idleThreshold: 10 * time.Minute, // Tier0 建议调高到 30 分钟
	}
	// 初始化 lastActivityAt 为当前时间
	s.lastActivityAt.Store(time.Now().UnixNano())
	return s
}

// MarkActivity 在任何 Admit 调用时更新最后活跃时间。
func (s *IdleEvolutionScheduler) MarkActivity() {
	s.lastActivityAt.Store(time.Now().UnixNano())
}

// WithConsolidate 注入巴固任务
func (s *IdleEvolutionScheduler) WithConsolidate(fn func(ctx context.Context) error) *IdleEvolutionScheduler {
	s.consolidateFn = fn
	return s
}

// WithForgetting 注入记忆滤波任务
func (s *IdleEvolutionScheduler) WithForgetting(fn func(ctx context.Context) error) *IdleEvolutionScheduler {
	s.forgettingFn = fn
	return s
}

// Run 启动调度器主循环，直到 ctx 被取消。
func (s *IdleEvolutionScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tryRunIdleTasks(ctx)
		}
	}
}

func (s *IdleEvolutionScheduler) isIdle() bool {
	lastNs := s.lastActivityAt.Load()
	idleDur := time.Since(time.Unix(0, lastNs))
	return idleDur > s.idleThreshold && s.rg.InFlight() == 0
}

func (s *IdleEvolutionScheduler) tryRunIdleTasks(ctx context.Context) {
	if !s.isIdle() {
		return
	}
	slog.InfoContext(ctx, "idle_evolution: idle window detected, starting background tasks")
	// Tier0 任务：巴固 + 记忆滤波
	if s.consolidateFn != nil {
		concurrent.SafeGo(ctx, "idle_evolution.consolidate", func(gctx context.Context) {
			if err := s.consolidateFn(gctx); err != nil {
				slog.WarnContext(gctx, "idle_evolution: consolidate failed", "err", err)
			}
		})
	}
	if s.forgettingFn != nil {
		concurrent.SafeGo(ctx, "idle_evolution.forgetting", func(gctx context.Context) {
			if err := s.forgettingFn(gctx); err != nil {
				slog.WarnContext(gctx, "idle_evolution: forgetting failed", "err", err)
			}
		})
	}
}
