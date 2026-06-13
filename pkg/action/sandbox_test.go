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

// ─── SandboxRouter 测试 ──────────────────────────────────────────────────────

func TestSandboxRouter_BuiltinGoesToInProcess(t *testing.T) {
	inProc := NewInProcessSandbox()
	inProc.Register("list-files", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte(`["a","b"]`), nil
	})
	router := NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 0)

	res, err := router.Execute(context.Background(), protocol.Tool{SandboxTier: 1, Name: "list-files"}, nil, protocol.TaintNone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success: %s", res.Error)
	}
}

// TestSandboxRouter_MCPFallsToInProcessWithoutContainer 验证无 Container 时 MCP 降级到 InProcess。
func TestSandboxRouter_MCPFallsToInProcessWithoutContainer(t *testing.T) {
	inProc := NewInProcessSandbox()
	inProc.Register("mcp-tool", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte(`{}`), nil
	})
	router := NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 0)

	res, err := router.Execute(context.Background(), protocol.Tool{SandboxTier: 3, Name: "mcp-tool"}, []byte("{}"), protocol.TaintNone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success")
	}
}

func TestAssignSandboxTier(t *testing.T) {
	tests := []struct {
		name       string
		source     protocol.ToolSource
		capability protocol.CapabilityLevel
		effects    []protocol.SideEffect
		hwTier     int
		goos       string
		wantTier   protocol.SandboxTier
	}{
		{"builtin-read", protocol.ToolBuiltin, protocol.CapReadOnly, nil, 1, "linux", protocol.SandboxInProcess},
		{"mcp-write", protocol.ToolMCP, protocol.CapWriteNetwork, nil, 1, "linux", protocol.SandboxWasm},
		{"llm-gen", protocol.ToolLLMGenerated, protocol.CapReadOnly, nil, 1, "linux", protocol.SandboxWasm},
		{"privileged-spawn", protocol.ToolBuiltin, protocol.CapPrivileged, []protocol.SideEffect{protocol.SideProcessSpawn}, 1, "linux", protocol.SandboxContainer},
		{"tier0-linux-container", protocol.ToolBuiltin, protocol.CapPrivileged, nil, 0, "linux", protocol.SandboxContainer},
		{"tier0-darwin-downgrade", protocol.ToolBuiltin, protocol.CapPrivileged, nil, 0, "darwin", protocol.SandboxWasm},
		{"tier1-darwin-no-downgrade", protocol.ToolBuiltin, protocol.CapPrivileged, nil, 1, "darwin", protocol.SandboxContainer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := protocol.Tool{
				Source:      tt.source,
				Capability:  tt.capability,
				SideEffects: tt.effects,
			}
			got := AssignSandboxTier(tool, tt.hwTier, tt.goos)
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
