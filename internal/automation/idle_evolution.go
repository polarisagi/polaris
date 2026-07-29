package automation

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

var (
	idleEvolutionTasksTotal = promauto.NewCounterVec( //nolint:gochecknoglobals
		prometheus.CounterOpts{
			Name: "idle_evolution_tasks_total",
			Help: "Total number of idle evolution tasks started.",
		},
		[]string{"task_type", "status"},
	)
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
	graphPruneFn  func(ctx context.Context) error // EdgeWeightManager.PeriodicPrune

	mu          sync.Mutex
	cancelFuncs []context.CancelFunc
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

// WithGraphPrune 注入图边裁剪任务
func (s *IdleEvolutionScheduler) WithGraphPrune(fn func(ctx context.Context) error) *IdleEvolutionScheduler {
	s.graphPruneFn = fn
	return s
}

// Run 启动调度器主循环，直到 ctx 被取消。
func (s *IdleEvolutionScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.cancelAll()
			return
		case <-ticker.C:
			if s.isIdle() {
				s.tryRunIdleTasks(ctx)
			} else {
				// 有新请求，立刻打断正在运行的 idle task
				s.cancelAll()
			}
		}
	}
}

func (s *IdleEvolutionScheduler) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.cancelFuncs {
		cancel()
	}
	s.cancelFuncs = nil
}

func (s *IdleEvolutionScheduler) isIdle() bool {
	lastNs := s.lastActivityAt.Load()
	idleDur := time.Since(time.Unix(0, lastNs))
	return idleDur > s.idleThreshold && s.rg.InFlight() == 0
}

func (s *IdleEvolutionScheduler) tryRunIdleTasks(ctx context.Context) {
	s.mu.Lock()
	if len(s.cancelFuncs) > 0 {
		// 已经在运行中
		s.mu.Unlock()
		return
	}
	slog.InfoContext(ctx, "idle_evolution: idle window detected, starting background tasks")

	taskCtx, cancel := context.WithCancel(ctx)
	s.cancelFuncs = append(s.cancelFuncs, cancel)
	s.mu.Unlock()

	// Tier0 任务：巴固 + 记忆滤波
	if s.consolidateFn != nil {
		idleEvolutionTasksTotal.WithLabelValues("consolidate", "started").Inc()
		concurrent.SafeGo(ctx, "idle_evolution.consolidate", func(gctx context.Context) {
			if err := s.consolidateFn(taskCtx); err != nil {
				slog.WarnContext(gctx, "idle_evolution: consolidate failed", "err", err)
				idleEvolutionTasksTotal.WithLabelValues("consolidate", "failed").Inc()
			} else {
				idleEvolutionTasksTotal.WithLabelValues("consolidate", "success").Inc()
			}
		})
	}
	if s.forgettingFn != nil {
		idleEvolutionTasksTotal.WithLabelValues("forgetting", "started").Inc()
		concurrent.SafeGo(ctx, "idle_evolution.forgetting", func(gctx context.Context) {
			if err := s.forgettingFn(taskCtx); err != nil {
				slog.WarnContext(gctx, "idle_evolution: forgetting failed", "err", err)
				idleEvolutionTasksTotal.WithLabelValues("forgetting", "failed").Inc()
			} else {
				idleEvolutionTasksTotal.WithLabelValues("forgetting", "success").Inc()
			}
		})
	}
	if s.graphPruneFn != nil {
		idleEvolutionTasksTotal.WithLabelValues("graph_prune", "started").Inc()
		concurrent.SafeGo(ctx, "idle_evolution.graph_prune", func(gctx context.Context) {
			if err := s.graphPruneFn(taskCtx); err != nil {
				slog.WarnContext(gctx, "idle_evolution: graph prune failed", "err", err)
				idleEvolutionTasksTotal.WithLabelValues("graph_prune", "failed").Inc()
			} else {
				idleEvolutionTasksTotal.WithLabelValues("graph_prune", "success").Inc()
			}
		})
	}
}
