package automation

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/automation/notify"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// collectingOutbox 是 protocol.OutboxWriter 的内存实现，收集所有写入条目供断言。
type collectingOutbox struct {
	mu      sync.Mutex
	entries []protocol.OutboxEntry
}

func (c *collectingOutbox) Write(_ context.Context, entry protocol.OutboxEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entry)
	return nil
}

func (c *collectingOutbox) snapshot() []protocol.OutboxEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]protocol.OutboxEntry, len(c.entries))
	copy(out, c.entries)
	return out
}

// waitForEntries 轮询等待 collectingOutbox 至少收到 n 条记录（scanAndDispatch 内
// 用 SafeGo 异步执行 dispatchFn，通知写入发生在该 goroutine 内，不能同步断言）。
func waitForEntries(t *testing.T, ob *collectingOutbox, n int) []protocol.OutboxEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if entries := ob.snapshot(); len(entries) >= n {
			return entries
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d outbox notification entries, got %d", n, len(ob.snapshot()))
	return nil
}

// TestSQLiteScheduler_NotifyTaskTerminal_BackgroundSuccess 验证 GD-13-001：
// 后台任务（Pool != intent_handler）成功终态时，写入一条 TopicNotification
// Outbox 条目。
func TestSQLiteScheduler_NotifyTaskTerminal_BackgroundSuccess(t *testing.T) {
	st := newMockStore()
	scheduler := NewSQLiteScheduler(st)
	ob := &collectingOutbox{}
	scheduler.WithOutboxWriter(ob)

	ctx := context.Background()
	task := types.Task{ID: "bg-success-1", Type: "cron", Pool: "background", MaxAttempts: 3}
	if _, err := scheduler.Submit(ctx, task); err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	scheduler.scanAndDispatch(ctx, func(ctx context.Context, task *types.Task) error {
		return nil
	})

	entries := waitForEntries(t, ob, 1)
	if entries[0].TargetEngine != protocol.TopicNotification {
		t.Fatalf("expected TargetEngine=%s, got %s", protocol.TopicNotification, entries[0].TargetEngine)
	}
	var ev notify.NotificationEvent
	if err := json.Unmarshal(entries[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal notification payload failed: %v", err)
	}
	if ev.TaskID != "bg-success-1" || !ev.Success || ev.Pool != "background" {
		t.Errorf("unexpected notification event: %+v", ev)
	}
}

// TestSQLiteScheduler_NotifyTaskTerminal_InteractiveTaskSkipped 验证用户交互式
// 任务（Pool=="intent_handler"，已有 SSE 实时可见）不重复发送通知。
func TestSQLiteScheduler_NotifyTaskTerminal_InteractiveTaskSkipped(t *testing.T) {
	st := newMockStore()
	scheduler := NewSQLiteScheduler(st)
	ob := &collectingOutbox{}
	scheduler.WithOutboxWriter(ob)

	ctx := context.Background()
	task := types.Task{ID: "interactive-1", Type: "chat", Pool: "intent_handler", MaxAttempts: 3}
	if _, err := scheduler.Submit(ctx, task); err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	scheduler.scanAndDispatch(ctx, func(ctx context.Context, task *types.Task) error {
		return nil
	})

	// 给异步 dispatch goroutine 一点时间执行，确认它确实没有写入任何通知。
	time.Sleep(200 * time.Millisecond)
	if entries := ob.snapshot(); len(entries) != 0 {
		t.Fatalf("expected no notification for intent_handler task, got %d entries", len(entries))
	}
}

// TestSQLiteScheduler_NotifyTaskTerminal_BackgroundFailure 验证后台任务耗尽重试
// 次数后（最终 failed 终态）也会写入通知，success=false。
func TestSQLiteScheduler_NotifyTaskTerminal_BackgroundFailure(t *testing.T) {
	st := newMockStore()
	scheduler := NewSQLiteScheduler(st)
	ob := &collectingOutbox{}
	scheduler.WithOutboxWriter(ob)

	ctx := context.Background()
	task := types.Task{ID: "bg-fail-1", Type: "cron", Pool: "background", MaxAttempts: 1}
	if _, err := scheduler.Submit(ctx, task); err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	alwaysFail := func(ctx context.Context, task *types.Task) error {
		return apperr.New(apperr.CodeInternal, "mock dispatch failure")
	}
	// 第一轮：CAS 为 running 并派发，dispatchFn 失败 → 回写 pending。
	scheduler.scanAndDispatch(ctx, alwaysFail)
	time.Sleep(100 * time.Millisecond)
	// 第二轮：Attempts(1) >= MaxAttempts(1) → 标记 failed 并发通知。
	scheduler.scanAndDispatch(ctx, alwaysFail)

	entries := waitForEntries(t, ob, 1)
	var ev notify.NotificationEvent
	if err := json.Unmarshal(entries[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal notification payload failed: %v", err)
	}
	if ev.TaskID != "bg-fail-1" || ev.Success {
		t.Errorf("expected failed notification for bg-fail-1, got: %+v", ev)
	}
}
