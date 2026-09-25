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
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

var (
	//custom-nolint:global-var
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

// launchIdleTask 启动一个空闲期后台任务。fn 为 nil 时不做任何事。
//
// 空闲判定（无用户活动 + InFlight==0）回答的是"现在该不该打扰用户"，
// 而 AdmitBackground 回答的是"现在机器扛不扛得住"——两者正交，都必须过。
// 此前只有前者：清库重启那种"用户没操作、但机器正在下模型 + 回填 148 个扩展
// 向量"的场景下，空闲判定为真，于是又把巩固/遗忘/剪枝一股脑压了上去。
func (s *IdleEvolutionScheduler) launchIdleTask(
	ctx, taskCtx context.Context, wg *sync.WaitGroup, name string, fn func(context.Context) error,
) {
	if fn == nil {
		return
	}
	release, ok := s.rg.AdmitBackground("idle_" + name)
	if !ok {
		idleEvolutionTasksTotal.WithLabelValues(name, "skipped_pressure").Inc()
		return
	}
	wg.Add(1)
	idleEvolutionTasksTotal.WithLabelValues(name, "started").Inc()
	concurrent.SafeGo(ctx, "idle_evolution."+name, func(gctx context.Context) {
		defer wg.Done()
		defer release()
		if err := fn(taskCtx); err != nil {
			slog.WarnContext(gctx, "idle_evolution: task failed", "task", name, "err", err)
			idleEvolutionTasksTotal.WithLabelValues(name, "failed").Inc()
			return
		}
		idleEvolutionTasksTotal.WithLabelValues(name, "success").Inc()
	})
}

func (s *IdleEvolutionScheduler) tryRunIdleTasks(ctx context.Context) {
	s.mu.Lock()
	if len(s.cancelFuncs) > 0 {
		// 已经在运行中
		s.mu.Unlock()
		return
	}

	// 如果没有任何任务注入，直接返回
	if s.consolidateFn == nil && s.forgettingFn == nil && s.graphPruneFn == nil {
		s.mu.Unlock()
		return
	}

	slog.InfoContext(ctx, "idle_evolution: idle window detected, starting background tasks")

	// 空闲任务的推理属于可降级后台工作：压力下挂起，且不刷新用户活跃时间。
	taskCtx, cancel := context.WithCancel(protocol.WithBackgroundWork(ctx))
	s.cancelFuncs = append(s.cancelFuncs, cancel)
	s.mu.Unlock()

	var wg sync.WaitGroup

	// Tier0 任务：巩固 + 记忆滤波 + 图剪枝。三者原为三段逐字重复的样板，
	// 2026-09-22 随资源准入接线一并收敛为 launchIdleTask。
	s.launchIdleTask(ctx, taskCtx, &wg, "consolidate", s.consolidateFn)
	s.launchIdleTask(ctx, taskCtx, &wg, "forgetting", s.forgettingFn)
	s.launchIdleTask(ctx, taskCtx, &wg, "graph_prune", s.graphPruneFn)

	concurrent.SafeGo(ctx, "idle_evolution.wait_cleanup", func(_ context.Context) {
		wg.Wait()
		cancel() // 释放资源

		s.mu.Lock()
		defer s.mu.Unlock()

		// 清理 cancelFuncs。注意这里只清理当前启动的 cancel，避免误删后续新生成的 cancel
		// 但最简单的是如果相等则置 nil，不过为了安全起见，通常 cancelFuncs 切片长度很小。
		// 由于 run 机制保证同一时间只有一个在运行（if len > 0 return），可以直接清空。
		s.cancelFuncs = nil
	})
}
