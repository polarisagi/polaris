package automation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/probe"
)

func TestIdleEvolutionScheduler_IsIdle(t *testing.T) {
	rg := newIdleMachineGovernor()
	hw := probe.NewHardwareProbe(0, 0)
	s := NewIdleEvolutionScheduler(rg, hw)

	// Set a very small threshold for testing
	s.idleThreshold = 50 * time.Millisecond

	// Initially, it should not be idle because lastActivityAt is just set
	if s.isIdle() {
		t.Error("Expected not to be idle immediately after creation")
	}

	// Move time back by 100ms
	s.lastActivityAt.Store(time.Now().Add(-100 * time.Millisecond).UnixNano())

	// Now it should be idle
	if !s.isIdle() {
		t.Error("Expected to be idle after threshold passed with 0 inflight")
	}

	// Simulate an active request
	rg.Admit(0)
	if s.isIdle() {
		t.Error("Expected not to be idle when InFlight > 0")
	}

	// Release request
	rg.Release()
	if !s.isIdle() {
		t.Error("Expected to be idle again when InFlight == 0")
	}

	// Mark activity updates the timestamp
	s.MarkActivity()
	if s.isIdle() {
		t.Error("Expected not to be idle immediately after MarkActivity")
	}
}

func TestIdleEvolutionScheduler_TaskCancelOnActivity(t *testing.T) {
	rg := newIdleMachineGovernor()
	hw := probe.NewHardwareProbe(0, 0)
	s := NewIdleEvolutionScheduler(rg, hw)
	s.idleThreshold = 10 * time.Millisecond

	taskStarted := make(chan struct{})
	taskCtxDone := make(chan struct{})

	// Inject a slow task
	s.WithConsolidate(func(ctx context.Context) error {
		close(taskStarted)
		<-ctx.Done()
		close(taskCtxDone)
		return ctx.Err()
	})

	// Make it idle
	s.lastActivityAt.Store(time.Now().Add(-100 * time.Millisecond).UnixNano())
	if !s.isIdle() {
		t.Fatal("Should be idle")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.tryRunIdleTasks(ctx)

	// Wait for task to start
	select {
	case <-taskStarted:
	case <-time.After(time.Second):
		t.Fatal("Task didn't start")
	}

	// Simulate new Admit which should cancel tasks (simulated via cancelAll in loop)
	rg.Admit(0) // Increases InFlight
	// Since we are not running the full `Run` loop (which ticks every 30s), we manually call what the loop would call
	if !s.isIdle() {
		s.cancelAll()
	}

	// Verify task is cancelled
	select {
	case <-taskCtxDone:
		// success
	case <-time.After(time.Second):
		t.Fatal("Task was not cancelled")
	}
}

func TestIdleEvolutionScheduler_NoDuplicateRun(t *testing.T) {
	rg := newIdleMachineGovernor()
	hw := probe.NewHardwareProbe(0, 0)
	s := NewIdleEvolutionScheduler(rg, hw)

	var runCount atomic.Int32
	s.WithForgetting(func(ctx context.Context) error {
		runCount.Add(1)
		<-ctx.Done()
		return nil
	})

	s.lastActivityAt.Store(time.Now().Add(-100 * time.Millisecond).UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.tryRunIdleTasks(ctx)
	s.tryRunIdleTasks(ctx) // Should be a no-op because tasks are already running

	// wait briefly for goroutines to start
	time.Sleep(50 * time.Millisecond)

	if runCount.Load() != 1 {
		t.Errorf("Expected 1 run, got %d", runCount.Load())
	}
}

func TestIdleEvolutionScheduler_NoLeakOnComplete(t *testing.T) {
	rg := newIdleMachineGovernor()
	hw := probe.NewHardwareProbe(0, 0)
	s := NewIdleEvolutionScheduler(rg, hw)

	var runCount atomic.Int32
	s.WithConsolidate(func(ctx context.Context) error {
		runCount.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.tryRunIdleTasks(ctx)

	// Wait for the wait_cleanup goroutine to finish
	time.Sleep(100 * time.Millisecond)

	s.mu.Lock()
	count := len(s.cancelFuncs)
	s.mu.Unlock()

	if count != 0 {
		t.Errorf("Expected cancelFuncs to be cleaned up, got %d", count)
	}
	if runCount.Load() != 1 {
		t.Errorf("Expected 1 run, got %d", runCount.Load())
	}
}

// newIdleMachineGovernor 构造探针恒报"空闲机器"的治理器。
//
// 调度器经 AdmitBackground 读取内存/CPU 探针；用真实探针时，测试结果随宿主负载
// 漂移——全仓 go test 并行编译把 CPU 打满，后台准入被拒，三个用例稳定失败，
// 空闲时单独跑又通过（2026-09-25 实测）。测试验证的是调度逻辑，不是宿主负载。
func newIdleMachineGovernor() *ResourceGovernor {
	rg := NewResourceGovernor(10, config.ResourceGovernorConfig{})
	rg.memProbeFn = func() int64 { return 64 * 1024 }
	rg.cpuProbeFn = func() float64 { return 0 }
	return rg
}

// 同一空闲窗口内任务跑完后不得再次启动（此前每 30s 重跑一次并刷日志）。
func TestIdleEvolutionScheduler_NoRerunWithoutNewActivity(t *testing.T) {
	rg := newIdleMachineGovernor()
	s := NewIdleEvolutionScheduler(rg, probe.NewHardwareProbe(0, 0))
	s.taskInterval = 0 // 隔离"新活动"这一条判据

	var runCount atomic.Int32
	s.WithForgetting(func(context.Context) error { runCount.Add(1); return nil })

	s.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.tryRunIdleTasks(ctx)
	time.Sleep(100 * time.Millisecond) // 等 wait_cleanup 清空 cancelFuncs
	s.tryRunIdleTasks(ctx)
	s.tryRunIdleTasks(ctx)
	time.Sleep(50 * time.Millisecond)
	if n := runCount.Load(); n != 1 {
		t.Fatalf("无新活动时应只跑 1 次，实际 %d", n)
	}

	// 新活动后再次空闲 → 放行
	s.lastActivityAt.Store(time.Now().Add(-30 * time.Minute).UnixNano())
	s.tryRunIdleTasks(ctx)
	time.Sleep(100 * time.Millisecond)
	if n := runCount.Load(); n != 2 {
		t.Fatalf("新活动后应再跑 1 次，实际 %d", n)
	}
}

// 活动/空闲抖动时，最小间隔内不重跑。
func TestIdleEvolutionScheduler_MinRunInterval(t *testing.T) {
	rg := newIdleMachineGovernor()
	s := NewIdleEvolutionScheduler(rg, probe.NewHardwareProbe(0, 0))

	var runCount atomic.Int32
	s.WithForgetting(func(context.Context) error { runCount.Add(1); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())
	s.tryRunIdleTasks(ctx)
	time.Sleep(100 * time.Millisecond)

	s.lastActivityAt.Store(time.Now().Add(-time.Hour + time.Second).UnixNano()) // 有新活动，但间隔未到
	s.tryRunIdleTasks(ctx)
	time.Sleep(50 * time.Millisecond)
	if n := runCount.Load(); n != 1 {
		t.Fatalf("最小间隔内不应重跑，实际 %d", n)
	}
}

type fakeJobState struct {
	mu     sync.Mutex // 调度器 goroutine 写、测试 goroutine 读
	last   map[string]time.Time
	status map[string]string
}

func (f *fakeJobState) GetLastRun(_ context.Context, job string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.last[job]
	return t, ok, nil
}

func (f *fakeJobState) RecordRun(_ context.Context, job, status string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[job] = at
	f.status[job] = status
	return nil
}

func (f *fakeJobState) statusOf(job string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[job]
}

func (f *fakeJobState) setLast(job string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[job] = at
}

func newFakeJobState() *fakeJobState {
	return &fakeJobState{last: map[string]time.Time{}, status: map[string]string{}}
}

// 探测无待处理数据 → 任务不启动，且记为一次已评估（重启后不再重复探测）。
func TestIdleEvolutionScheduler_ProbeNoWorkSkips(t *testing.T) {
	rg := newIdleMachineGovernor()
	st := newFakeJobState()
	s := NewIdleEvolutionScheduler(rg, probe.NewHardwareProbe(0, 0)).WithStateRepo(st)

	var runCount atomic.Int32
	s.WithForgetting(func(context.Context) error { runCount.Add(1); return nil }).
		WithForgettingProbe(func(context.Context) (bool, error) { return false, nil })

	s.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())
	s.tryRunIdleTasks(context.Background())
	time.Sleep(50 * time.Millisecond)

	if runCount.Load() != 0 {
		t.Fatalf("无待处理数据时不应启动任务，实际 %d", runCount.Load())
	}
	if got := st.statusOf("idle_forgetting"); got != "no_work" {
		t.Fatalf("应落库 no_work，实际 %q", got)
	}
}

// 库里已有近期记录（模拟进程重启）→ 不启动；记录过期 → 启动并落库 success。
func TestIdleEvolutionScheduler_PersistedStateSurvivesRestart(t *testing.T) {
	st := newFakeJobState()
	st.setLast("idle_forgetting", time.Now().Add(-time.Hour))

	newSched := func(runCount *atomic.Int32) *IdleEvolutionScheduler {
		s := NewIdleEvolutionScheduler(newIdleMachineGovernor(), probe.NewHardwareProbe(0, 0)).WithStateRepo(st)
		s.WithForgetting(func(context.Context) error { runCount.Add(1); return nil })
		s.lastActivityAt.Store(time.Now().Add(-time.Hour).UnixNano())
		return s
	}

	var n1 atomic.Int32
	newSched(&n1).tryRunIdleTasks(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n1.Load() != 0 {
		t.Fatalf("库中 1 小时前已跑过，重启后不应重跑，实际 %d", n1.Load())
	}

	st.setLast("idle_forgetting", time.Now().Add(-48*time.Hour))
	var n2 atomic.Int32
	newSched(&n2).tryRunIdleTasks(context.Background())
	time.Sleep(100 * time.Millisecond)
	if n2.Load() != 1 {
		t.Fatalf("记录过期应启动，实际 %d", n2.Load())
	}
	if st.statusOf("idle_forgetting") != "success" {
		t.Fatalf("应落库 success，实际 %q", st.statusOf("idle_forgetting"))
	}
}
