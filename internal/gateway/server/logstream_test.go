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

	if len(ls.subs) != 0 {
		t.Errorf("expected empty subs")
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
