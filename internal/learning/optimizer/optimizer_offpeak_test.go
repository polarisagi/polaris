package optimizer

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeOffPeak：Allow 恒 false；Wait 阻塞到 open 被关闭或 ctx 取消。
type fakeOffPeak struct{ open chan struct{} }

func (f *fakeOffPeak) Allow() bool { return false }
func (f *fakeOffPeak) Wait(ctx context.Context) error {
	select {
	case <-f.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func queuedLen(po *PromptOptimizer) int {
	po.offPeakMu.Lock()
	defer po.offPeakMu.Unlock()
	return len(po.offPeakQueued)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("条件在 2s 内未满足")
}

// 窗口外：OptimizeTask 立即返回（不阻塞触发方），同 taskType 的重复触发合并；窗口开启后待办清空。
func TestOptimizeTask_DefersOutsideWindowAndCoalesces(t *testing.T) {
	gate := &fakeOffPeak{open: make(chan struct{})}
	po := NewPromptOptimizer(nil, nil, 0).WithOffPeak(context.Background(), gate)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = po.OptimizeTask(context.Background(), "qa")
		_ = po.OptimizeTask(context.Background(), "qa")
		_ = po.OptimizeTask(context.Background(), "code")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("窗口外的 OptimizeTask 不得阻塞调用方")
	}
	if n := queuedLen(po); n != 2 {
		t.Fatalf("qa 重复触发应合并，待办应为 2（qa, code），got %d", n)
	}
	close(gate.open)
	waitFor(t, func() bool { return queuedLen(po) == 0 })
}

// 进程退出（生命周期 ctx 取消）：等待窗口的协程退出、待办丢弃，不留泄漏。
func TestOptimizeTask_LifecycleCancelDropsPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	po := NewPromptOptimizer(nil, nil, 0).WithOffPeak(ctx, &fakeOffPeak{open: make(chan struct{})})
	_ = po.OptimizeTask(context.Background(), "qa")
	if queuedLen(po) != 1 {
		t.Fatal("应登记待办")
	}
	cancel()
	waitFor(t, func() bool { return queuedLen(po) == 0 })
}

// 未启用错峰（nil gate）：行为与改动前一致，同步执行、不登记待办。
func TestOptimizeTask_NoGateRunsInline(t *testing.T) {
	po := NewPromptOptimizer(nil, nil, 0)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = po.OptimizeTask(context.Background(), "qa") }()
	wg.Wait()
	if queuedLen(po) != 0 {
		t.Fatal("未启用错峰不应登记待办")
	}
}
