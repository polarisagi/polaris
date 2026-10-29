package server

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

func TestLogStreamEnabled(t *testing.T) {
	ls := NewLogStore(slog.NewJSONHandler(os.Stdout, nil), 100)
	if !ls.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("expected true")
	}
}

func TestLogStreamWithGroup(t *testing.T) {
	ls := NewLogStore(slog.NewJSONHandler(os.Stdout, nil), 100)
	_ = ls.WithGroup("group")
}

func TestLogStreamWithAttrs(t *testing.T) {
	ls := NewLogStore(slog.NewJSONHandler(os.Stdout, nil), 100)
	_ = ls.WithAttrs([]slog.Attr{{Key: "k", Value: slog.StringValue("v")}})
}
