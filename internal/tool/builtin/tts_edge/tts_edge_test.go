package tts_edge

import (
	"context"
	"testing"
)

func TestExecuteEdgeTTS_InvalidArgs(t *testing.T) {
	fn := MakeExecuteEdgeTTSFn(false, "")
	if _, err := fn(context.Background(), []byte("invalid")); err == nil {
		t.Fatal("expected error")
	}
}

// edge-tts CLI 不可用时必须如实返回错误，不得回出假 MP3 并报 success（静默兜底）。
func TestExecuteEdgeTTS_FailureIsNotMasked(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // 保证找不到 edge-tts
	fn := MakeExecuteEdgeTTSFn(false, "")
	out, err := fn(context.Background(), []byte(`{"text":"test"}`))
	if err == nil {
		t.Fatalf("expected error when edge-tts is unavailable, got output: %s", out)
	}
}
