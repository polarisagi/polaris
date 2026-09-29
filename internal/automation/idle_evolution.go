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
	"github.com/polarisagi/polaris/internal/protocol/repo"
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

const (
	// idleTaskInterval 同一任务两次评估的最小间隔。遗忘衰减与图边剪枝都是按天粒度的
	// 存量清理（PeriodicPrune 的注释即"每日"），更频繁只是空扫库。
	idleTaskInterval = 24 * time.Hour

	jobStatusSuccess = "success"
	jobStatusNoWork  = "no_work"
)

// idleTask 一个可被空闲调度的后台任务。
type idleTask struct {
	name string
	fn   func(ctx context.Context) error
	// probe 只读探测"是否有待处理数据"。nil 视为有活；探测出错按有活处理（fail-open，
	// 宁可多跑一次幂等清理，也不因探测故障让清理永远不跑）。
	probe func(ctx context.Context) (bool, error)
}

// IdleEvolutionScheduler 在系统空闲期间主动触发记忆巩固、遗忘和图剪枝任务。
// 空闲判定： time.Since(lastActivityAt) > idleThreshold && rg.InFlight()==0
//
// 是否真的启动由三道门依次决定，任何一道不过都不启动、不打日志：
//  1. 窗口门：自上次评估以来没有新的用户活动 → 不再评估（同一空闲窗口只评估一次）。
//  2. 间隔门：该任务距上次成功评估不足 taskInterval（状态落库，重启不丢）。
//  3. 数据门：只读探测确认存在待处理数据（probe）。
type IdleEvolutionScheduler struct {
	rg             *ResourceGovernor
	hw             *probe.HardwareProbe // 用于 Tier 门控
	idleThreshold  time.Duration
	taskInterval   time.Duration // 单任务两次评估的最小间隔
	lastActivityAt atomic.Int64  // Unix 纳秒，由 ResourceGovernor.Admit 更新
	// 可被注入的任务（Tier0 默认开启）
	consolidateFn func(ctx context.Context) error // consolidation.ConsolidationPipeline.Consolidate
	forgettingFn  func(ctx context.Context) error // ForgettingManager.PeriodicCleanup
	graphPruneFn  func(ctx context.Context) error // EdgeWeightManager.PeriodicPrune

	forgettingProbe func(ctx context.Context) (bool, error)
	graphPruneProbe func(ctx context.Context) (bool, error)

	state repo.BackgroundJobStateRepository // 可选：nil 时仅内存记录，重启后重新评估

	mu          sync.Mutex
	cancelFuncs []context.CancelFunc
	// 窗口门状态（mu 保护）：上次评估时的用户活动戳与时间。
	checked             bool
	lastCheckedActivity int64
	lastCheckedAt       time.Time
	// state 缺席或读写失败时的内存兜底（mu 保护）。
	lastRunMem map[string]time.Time
}

func NewIdleEvolutionScheduler(rg *ResourceGovernor, hw *probe.HardwareProbe) *IdleEvolutionScheduler {
	s := &IdleEvolutionScheduler{
		rg:            rg,
		hw:            hw,
		idleThreshold: 10 * time.Minute, // Tier0 建议调高到 30 分钟
		taskInterval:  idleTaskInterval,
		lastRunMem:    make(map[string]time.Time),
	}
	// 初始化 lastActivityAt 为当前时间
	s.lastActivityAt.Store(time.Now().UnixNano())
	return s
}

// MarkActivity 在任何 Admit 调用时更新最后活跃时间。
func (s *IdleEvolutionScheduler) MarkActivity() {
	s.lastActivityAt.Store(time.Now().UnixNano())
}

// WithConsolidate 注入巩固任务
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

// WithForgettingProbe 注入遗忘任务的"有无待处理数据"只读探测。
func (s *IdleEvolutionScheduler) WithForgettingProbe(fn func(ctx context.Context) (bool, error)) *IdleEvolutionScheduler {
	s.forgettingProbe = fn
	return s
}

// WithGraphPruneProbe 注入图剪枝任务的"有无待处理数据"只读探测。
func (s *IdleEvolutionScheduler) WithGraphPruneProbe(fn func(ctx context.Context) (bool, error)) *IdleEvolutionScheduler {
	s.graphPruneProbe = fn
	return s
}

// WithStateRepo 注入任务运行状态持久化（041_background_job_state）。
func (s *IdleEvolutionScheduler) WithStateRepo(r repo.BackgroundJobStateRepository) *IdleEvolutionScheduler {
	s.state = r
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

// registeredTasks 返回已注入的任务（固定顺序）。
func (s *IdleEvolutionScheduler) registeredTasks() []idleTask {
	var tasks []idleTask
	if s.consolidateFn != nil {
		tasks = append(tasks, idleTask{name: "consolidate", fn: s.consolidateFn})
	}
	if s.forgettingFn != nil {
		tasks = append(tasks, idleTask{name: "forgetting", fn: s.forgettingFn, probe: s.forgettingProbe})
	}
	if s.graphPruneFn != nil {
		tasks = append(tasks, idleTask{name: "graph_prune", fn: s.graphPruneFn, probe: s.graphPruneProbe})
	}
	return tasks
}

func jobKey(name string) string { return "idle_" + name }

// lastRun 返回任务上次成功评估时间：优先内存（本进程内最新），其次库。
func (s *IdleEvolutionScheduler) lastRun(ctx context.Context, name string) (time.Time, bool) {
	s.mu.Lock()
	t, ok := s.lastRunMem[name]
	s.mu.Unlock()
	if ok {
		return t, true
	}
	if s.state == nil {
		return time.Time{}, false
	}
	at, found, err := s.state.GetLastRun(ctx, jobKey(name))
	if err != nil {
		slog.DebugContext(ctx, "idle_evolution: read job state failed", "task", name, "err", err)
		return time.Time{}, false
	}
	return at, found
}

// recordRun 记录一次成功评估（内存 + 库）。库写失败只降级为仅内存，不影响任务本身。
func (s *IdleEvolutionScheduler) recordRun(ctx context.Context, name, status string) {
	now := time.Now()
	s.mu.Lock()
	s.lastRunMem[name] = now
	s.mu.Unlock()
	if s.state == nil {
		return
	}
	if err := s.state.RecordRun(ctx, jobKey(name), status, now); err != nil {
		slog.WarnContext(ctx, "idle_evolution: persist job state failed", "task", name, "err", err)
	}
}

// dueTasks 依次过间隔门与数据门，返回真正需要启动的任务。
// 探测出"无活"的任务同样记一次评估，避免重启后立刻重复探测。
func (s *IdleEvolutionScheduler) dueTasks(ctx context.Context, tasks []idleTask) []idleTask {
	due := make([]idleTask, 0, len(tasks))
	for _, t := range tasks {
		if last, ok := s.lastRun(ctx, t.name); ok && time.Since(last) < s.taskInterval {
			idleEvolutionTasksTotal.WithLabelValues(t.name, "skipped_recent").Inc()
			continue
		}
		if t.probe != nil {
			has, err := t.probe(ctx)
			if err != nil {
				slog.DebugContext(ctx, "idle_evolution: probe failed, assume work", "task", t.name, "err", err)
			} else if !has {
				idleEvolutionTasksTotal.WithLabelValues(t.name, "skipped_no_work").Inc()
				s.recordRun(ctx, t.name, jobStatusNoWork)
				continue
			}
		}
		due = append(due, t)
	}
	return due
}

// launchIdleTask 启动一个空闲期后台任务，返回是否真的启动。
//
// 空闲判定（无用户活动 + InFlight==0）回答的是"现在该不该打扰用户"，
// 而 AdmitBackground 回答的是"现在机器扛不扛得住"——两者正交，都必须过。
// 此前只有前者：清库重启那种"用户没操作、但机器正在下模型 + 回填 148 个扩展
// 向量"的场景下，空闲判定为真，于是又把巩固/遗忘/剪枝一股脑压了上去。
func (s *IdleEvolutionScheduler) launchIdleTask(
	ctx, taskCtx context.Context, wg *sync.WaitGroup, name string, fn func(context.Context) error,
) bool {
	if fn == nil {
		return false
	}
	release, ok := s.rg.AdmitBackground("idle_" + name)
	if !ok {
		idleEvolutionTasksTotal.WithLabelValues(name, "skipped_pressure").Inc()
		return false
	}
	wg.Add(1)
	idleEvolutionTasksTotal.WithLabelValues(name, "started").Inc()
	concurrent.SafeGo(ctx, "idle_evolution."+name, func(gctx context.Context) {
		defer wg.Done()
		defer release()
		if err := fn(taskCtx); err != nil {
			slog.WarnContext(gctx, "idle_evolution: task failed", "task", name, "err", err)
			idleEvolutionTasksTotal.WithLabelValues(name, "failed").Inc()
			return // 失败不记评估：下个窗口重试
		}
		idleEvolutionTasksTotal.WithLabelValues(name, "success").Inc()
		s.recordRun(gctx, name, jobStatusSuccess)
	})
	return true
}

func (s *IdleEvolutionScheduler) tryRunIdleTasks(ctx context.Context) {
	s.mu.Lock()
	if len(s.cancelFuncs) > 0 {
		// 已经在运行中
		s.mu.Unlock()
		return
	}
	tasks := s.registeredTasks()
	if len(tasks) == 0 {
		s.mu.Unlock()
		return
	}
	// 窗口门：无新活动且未到重评估时间，本窗口已评估过。
	activity := s.lastActivityAt.Load()
	if s.checked && activity == s.lastCheckedActivity && time.Since(s.lastCheckedAt) < idleTaskInterval {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	due := s.dueTasks(ctx, tasks)

	s.mu.Lock()
	s.checked = true
	s.lastCheckedActivity = activity
	s.lastCheckedAt = time.Now()
	if len(due) == 0 {
		s.mu.Unlock()
		return
	}

	// 空闲任务的推理属于可降级后台工作：压力下挂起，且不刷新用户活跃时间。
	taskCtx, cancel := context.WithCancel(protocol.WithBackgroundWork(ctx))
	s.cancelFuncs = append(s.cancelFuncs, cancel)
	s.mu.Unlock()

	var wg sync.WaitGroup
	launched := 0
	for _, t := range due {
		if s.launchIdleTask(ctx, taskCtx, &wg, t.name, t.fn) {
			launched++
		}
	}

	// 全被资源压力拒绝：什么都没启动，撤销本次评估记录，下个 tick 再试。
	if launched == 0 {
		cancel()
		s.mu.Lock()
		s.cancelFuncs = nil
		s.checked = false
		s.mu.Unlock()
		return
	}
	slog.InfoContext(ctx, "idle_evolution: idle window detected, started background tasks", "tasks", launched)

	concurrent.SafeGo(ctx, "idle_evolution.wait_cleanup", func(_ context.Context) {
		wg.Wait()
		cancel() // 释放资源

		s.mu.Lock()
		defer s.mu.Unlock()

		// 由 len(cancelFuncs)>0 → return 保证同一时间只有一批在运行，可直接清空。
		s.cancelFuncs = nil
	})
}
