package protocol

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestInferOptions(t *testing.T) {
	opts := ApplyInferOptions([]types.InferOption{
		types.WithThinkingMode("fast"),
		types.WithMaxTokens(200),
		types.WithModel("test-model"),
		types.WithTemperature(0.5),
		types.WithTopP(0.9),
	})

	if opts.ThinkingMode != "fast" {
		t.Errorf("Expected fast, got %s", opts.ThinkingMode)
	}
	if opts.MaxTokens != 200 {
		t.Errorf("Expected 200, got %d", opts.MaxTokens)
	}
	if opts.Model != "test-model" {
		t.Errorf("Expected test-model, got %s", opts.Model)
	}
	if opts.Temperature != 0.5 {
		t.Errorf("Expected 0.5, got %v", opts.Temperature)
	}
	if opts.TopP != 0.9 {
		t.Errorf("Expected 0.9, got %v", opts.TopP)
	}
}

func TestTaintLevel_String(t *testing.T) {
	tests := []struct {
		level    types.TaintLevel
		expected string
	}{
		{types.TaintNone, "none"},
		{types.TaintMedium, "medium"},
		{types.TaintHigh, "high"},
		{types.TaintLevel(99), "unknown"},
	}

	for _, tc := range tests {
		if got := tc.level.String(); got != tc.expected {
			t.Errorf("Expected %s, got %s", tc.expected, got)
		}
	}
}

func TestPropagateTaint(t *testing.T) {
	if got := types.PropagateTaint(types.TaintNone, types.TaintMedium); got != types.TaintMedium {
		t.Errorf("Expected Medium, got %v", got)
	}
	if got := types.PropagateTaint(types.TaintNone, types.TaintNone); got != types.TaintNone {
		t.Errorf("Expected None, got %v", got)
	}
	if got := types.PropagateTaint(types.TaintHigh, types.TaintMedium); got != types.TaintHigh {
		t.Errorf("Expected High, got %v", got)
	}
}

func TestBuildIdempotencyKey(t *testing.T) {
	key := types.BuildIdempotencyKey("engine", "type", "id", "op", 1)
	expected := "engine:type:id:op:1"
	if string(key) != expected {
		t.Errorf("Expected %s, got %s", expected, key)
	}
}
