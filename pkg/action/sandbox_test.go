package action

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── InProcessSandbox 测试 ────────────────────────────────────────────────────

func TestInProcessSandbox_RegisterAndRun(t *testing.T) {
	sb := NewInProcessSandbox()
	sb.Register("echo", func(_ context.Context, input []byte) ([]byte, error) {
		return append([]byte("echo:"), input...), nil
	})

	result, err := sb.Run(context.Background(), SandboxSpec{
		ToolName: "echo",
		Input:    []byte("hello"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
	if string(result.Output) != "echo:hello" {
		t.Fatalf("unexpected output: %s", result.Output)
	}
}

func TestInProcessSandbox_UnknownTool(t *testing.T) {
	sb := NewInProcessSandbox()
	_, err := sb.Run(context.Background(), SandboxSpec{ToolName: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
}

func TestInProcessSandbox_Timeout(t *testing.T) {
	sb := NewInProcessSandbox()
	sb.Register("slow", func(ctx context.Context, _ []byte) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return []byte("ok"), nil
		}
	})

	result, err := sb.Run(context.Background(), SandboxSpec{
		ToolName:   "slow",
		CPUQuotaMs: 50, // 50ms 超时
	})
	// 超时返回 ToolResult.Success=false 而非 error
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatal("expected timeout failure")
	}
}

// ─── WasmSandbox 测试 ─────────────────────────────────────────────────────────

func TestWasmSandbox_StubExecution(t *testing.T) {
	sb := NewWasmSandbox(context.Background(), 4)
	emptyWasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	result, err := sb.Run(context.Background(), SandboxSpec{
		ToolName:    "test-tool",
		Input:       []byte(`{"key":"value"}`),
		SandboxTier: protocol.SandboxWasm,
		WasmBytes:   emptyWasm,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}
}

// ─── SandboxRouter 测试 ──────────────────────────────────────────────────────

func TestSandboxRouter_BuiltinGoesToInProcess(t *testing.T) {
	inProc := NewInProcessSandbox()
	inProc.Register("list-files", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte(`["a","b"]`), nil
	})
	router := NewSandboxRouter(inProc, NewWasmSandbox(context.Background(), 4), nil, runtime.GOOS, 0)

	tool := protocol.Tool{
		Name:        "list-files",
		Source:      protocol.ToolBuiltin,
		Capability:  protocol.CapReadOnly,
		SideEffects: []protocol.SideEffect{protocol.SideNone},
	}

	result, err := router.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success: %s", result.Error)
	}
}

func TestSandboxRouter_MCPGoesToWasm(t *testing.T) {
	wasmSb := NewWasmSandbox(context.Background(), 4)
	emptyWasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	wasmSb.PreWarmCache("mcp-tool", emptyWasm)

	router := NewSandboxRouter(NewInProcessSandbox(), wasmSb, nil, runtime.GOOS, 0)

	tool := protocol.Tool{
		Name:       "mcp-tool",
		Source:     protocol.ToolMCP,
		Capability: protocol.CapWriteNetwork,
	}

	result, err := router.Execute(context.Background(), tool, []byte("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
}

func TestSandboxRouter_PrivilegedGoesToWasmOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("skip non-linux fallback test on Linux")
	}
	router := NewSandboxRouter(NewInProcessSandbox(), NewWasmSandbox(context.Background(), 4), nil, runtime.GOOS, 0)

	tool := protocol.Tool{
		Name:        "sys-tool",
		Source:      protocol.ToolBuiltin,
		Capability:  protocol.CapPrivileged,
		SideEffects: []protocol.SideEffect{protocol.SideProcessSpawn},
	}

	provider := router.Route(tool)
	if _, ok := provider.(*WasmSandbox); !ok {
		t.Fatalf("expected WasmSandbox on non-Linux, got %T", provider)
	}
}

func TestAssignSandboxTier(t *testing.T) {
	tests := []struct {
		name       string
		source     protocol.ToolSource
		capability protocol.CapabilityLevel
		effects    []protocol.SideEffect
		wantTier   protocol.SandboxTier
	}{
		{"builtin-read", protocol.ToolBuiltin, protocol.CapReadOnly, nil, protocol.SandboxInProcess},
		{"mcp-write", protocol.ToolMCP, protocol.CapWriteNetwork, nil, protocol.SandboxWasm},
		{"llm-gen", protocol.ToolLLMGenerated, protocol.CapReadOnly, nil, protocol.SandboxWasm},
		{"privileged-spawn", protocol.ToolBuiltin, protocol.CapPrivileged, []protocol.SideEffect{protocol.SideProcessSpawn}, protocol.SandboxWasm}, // non-Linux
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := protocol.Tool{
				Source:      tt.source,
				Capability:  tt.capability,
				SideEffects: tt.effects,
			}
			got := AssignSandboxTier(tool, 0, "darwin")
			if got != tt.wantTier {
				t.Errorf("expected tier %d, got %d", tt.wantTier, got)
			}
		})
	}
}

func TestNoopReadCloser(t *testing.T) {
	r := bytes2ReadCloser([]byte("hello world"))
	buf := make([]byte, 5)
	n, _ := r.Read(buf)
	if n != 5 || string(buf) != "hello" {
		t.Fatalf("expected 'hello', got %q", buf)
	}
	n2, _ := r.Read(buf)
	if n2 != 5 || !strings.HasPrefix(string(buf), " worl") {
		t.Fatalf("expected ' worl', got %q", buf)
	}
}
