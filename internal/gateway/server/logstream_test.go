package server

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestLogStore(t *testing.T) {
	ls := NewLogStore(slog.Default().Handler(), 10)

	id, ch := ls.Subscribe()

	// Simulate log
	ls.Handle(context.Background(), slog.Record{
		Time:    time.Now(),
		Level:   slog.LevelInfo,
		Message: "test message",
	})

	select {
	case entry := <-ch:
		if entry.Message != "test message" || entry.Level != "info" {
			t.Errorf("unexpected entry: %+v", entry)
		}
	case <-time.After(time.Second):
		t.Errorf("timeout waiting for log entry")
	}

	recent := ls.Recent()
	if len(recent) != 1 {
		t.Errorf("expected 1 recent, got %d", len(recent))
	}

	ls.Unsubscribe(id)

	if len(ls.shared.subs) != 0 {
		t.Errorf("expected empty subs")
	}
}

// TestLogStore_WithAttrsPreservesRingBufferAndSSE 验证 logger.With(...)/WithGroup(...)
// 产生的子 Handler 仍然写入同一份环形缓冲并广播给同一批 SSE 订阅者，而不是像
// 修复前那样剥离 LogStore 包装、绕过 /v1/logs/stream（ADR-0094 决策七）。
func TestLogStore_WithAttrsPreservesRingBufferAndSSE(t *testing.T) {
	ls := NewLogStore(slog.Default().Handler(), 10)

	child := ls.WithAttrs([]slog.Attr{slog.String("component", "test")})
	childLS, ok := child.(*LogStore)
	if !ok {
		t.Fatalf("expected WithAttrs to return *LogStore, got %T", child)
	}
	if childLS.shared != ls.shared {
		t.Fatalf("expected child LogStore to share the same ring buffer/subs as parent")
	}

	_, ch := ls.Subscribe()

	if err := childLS.Handle(context.Background(), slog.Record{
		Time:    time.Now(),
		Level:   slog.LevelInfo,
		Message: "from child logger",
	}); err != nil {
		t.Fatalf("child Handle failed: %v", err)
	}

	select {
	case entry := <-ch:
		if entry.Message != "from child logger" {
			t.Errorf("unexpected entry: %+v", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: child logger's log entry never reached parent's SSE subscriber")
	}

	group := ls.WithGroup("mygroup")
	groupLS, ok := group.(*LogStore)
	if !ok {
		t.Fatalf("expected WithGroup to return *LogStore, got %T", group)
	}
	if groupLS.shared != ls.shared {
		t.Fatalf("expected WithGroup LogStore to share the same ring buffer/subs as parent")
	}
}

func TestLevelGe(t *testing.T) {
	if !levelGe("info", "info") {
		t.Errorf("info >= info")
	}
	if !levelGe("error", "warn") {
		t.Errorf("error >= warn")
	}
	if levelGe("debug", "info") {
		t.Errorf("debug < info")
	}
}
