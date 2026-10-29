package sandbox

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/sandbox"
)

func TestWasmtimeSandbox(t *testing.T) {
	s := NewWasmtimeSandbox("/tmp")
	if s.Level() != 2 {
		t.Fatalf("expected level 2")
	}

	spec := sandbox.SandboxSpec{
		DryRunMode: true,
		ToolName:   "test-tool",
	}
	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error")
	}
	if !res.Success || string(res.Output) != `{"mocked": true, "tool": "test-tool"}` {
		t.Fatalf("unexpected mock result")
	}

	specReal := sandbox.SandboxSpec{
		DryRunMode:  false,
		ScriptBytes: []byte("dummy"),
		Input:       []byte("input"),
	}
	resReal, errReal := s.Run(context.Background(), specReal)
	_ = resReal
	_ = errReal
}
