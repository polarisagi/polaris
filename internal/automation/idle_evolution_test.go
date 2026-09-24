package automation

import (
	"context"
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
