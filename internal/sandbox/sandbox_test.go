package sandbox

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
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

	res, err := router.Execute(context.Background(), types.Tool{SandboxTier: 1, Name: "list-files"}, nil, types.TaintNone)
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

	res, err := router.Execute(context.Background(), types.Tool{SandboxTier: 3, Name: "mcp-tool"}, []byte("{}"), types.TaintNone)
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
		source     types.ToolSource
		capability types.CapabilityLevel
		effects    []types.SideEffect
		hwTier     int
		goos       string
		wantTier   types.SandboxTier
		wantErr    error
	}{
		{"builtin-read", types.ToolBuiltin, types.CapReadOnly, nil, 1, "linux", types.SandboxInProcess, nil},
		{"mcp-write", types.ToolMCP, types.CapWriteNetwork, nil, 1, "linux", types.SandboxWasm, nil},
		{"llm-gen", types.ToolLLMGenerated, types.CapReadOnly, nil, 1, "linux", types.SandboxWasm, nil},
		{"privileged-spawn", types.ToolBuiltin, types.CapPrivileged, []types.SideEffect{types.SideProcessSpawn}, 1, "linux", types.SandboxContainer, nil},
		{"tier0-linux-container", types.ToolBuiltin, types.CapPrivileged, nil, 0, "linux", types.SandboxInProcess, apperr.ErrTier0SandboxLimit},
		{"tier0-darwin-downgrade", types.ToolBuiltin, types.CapPrivileged, nil, 0, "darwin", types.SandboxInProcess, apperr.ErrTier0SandboxLimit},
		{"tier1-darwin-no-downgrade", types.ToolBuiltin, types.CapPrivileged, nil, 1, "darwin", types.SandboxContainer, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := types.Tool{
				Source:      tt.source,
				Capability:  tt.capability,
				SideEffects: tt.effects,
			}
			got, err := AssignSandboxTier(tool, tt.hwTier, tt.goos)
			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Errorf("expected error %v, got %v", tt.wantErr, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if got != tt.wantTier {
					t.Errorf("expected tier %d, got %d", tt.wantTier, got)
				}
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

// ─── Additional InProcessSandbox tests ──────────────────────────────────────

func TestInProcessSandbox_AdditionalMethods(t *testing.T) {
	sb := NewInProcessSandbox()

	// RegisterWithTaint
	sb.RegisterWithTaint("tainted", func(ctx context.Context, in []byte) ([]byte, error) {
		return in, nil
	}, types.TaintMedium)

	// RegisterRich
	sb.RegisterRich("rich", func(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
		return &types.ToolResult{Success: true, Output: spec.Input}, nil
	}, types.TaintNone)

	// Unregister
	sb.Unregister("tainted")

	// Check if tainted is removed
	_, err := sb.Run(context.Background(), SandboxSpec{ToolName: "tainted"})
	if err == nil {
		t.Fatal("expected error for unregistered tool")
	}

	// Run rich tool
	res, err := sb.Run(context.Background(), SandboxSpec{ToolName: "rich", Input: []byte("test")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success")
	}
}

// ─── ContainerSandbox Tests (Anomaly Paths) ──────────────────────────────────

func TestContainerSandbox_Methods(t *testing.T) {
	sb := NewContainerSandbox("/usr/local/bin/polaris-sandbox", "darwin", 1)

	if sb.Level() != int(types.SandboxContainer) {
		t.Errorf("expected Container level %v, got %v", types.SandboxContainer, sb.Level())
	}

	// Run without tool -> should fail (not implemented or execution error)
	res, err := sb.Run(context.Background(), SandboxSpec{ToolName: "bash"})
	// It should either return error or success=false
	if err == nil && res.Success {
		t.Error("expected container run to fail in test environment")
	}

	// RunScript
	_ = sb.RunHook(context.Background(), "echo", "/tmp")
}

// ─── SandboxRouter Additional Tests ──────────────────────────────────────────

func TestSandboxRouter_AnomalyPaths(t *testing.T) {
	inProc := NewInProcessSandbox()
	router := NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 1) // HwTier=1

	// Route should fall back appropriately
	tool := types.Tool{
		Source:      types.ToolBuiltin,
		Capability:  types.CapReadOnly,
		SandboxTier: 1, // Builtin ReadOnly -> InProcess
	}

	// Execute without provider (inProc has no tool registered)
	_, err := router.Execute(context.Background(), tool, nil, types.TaintNone)
	if err == nil {
		t.Error("expected error executing when no sandbox tool available")
	}

	// Route to SandboxContainer when container is nil should fallback
	toolContainer := types.Tool{
		Source:      types.ToolBuiltin,
		Capability:  types.CapPrivileged,
		SideEffects: []types.SideEffect{types.SideProcessSpawn},
		SandboxTier: 3, // -> SandboxContainer
	}
	// On darwin hwTier 1 -> SandboxContainer. But router.container is nil.
	// Route should fallback to remote or inProcess.
	provider, err := router.Route(toolContainer)
	if err != nil {
		t.Errorf("expected no error routing, got: %v", err)
	}
	if provider != inProc {
		t.Errorf("expected fallback to inProcess, got %v", provider)
	}

	// Test Kill/Disable methods (should not panic)
	router.DisableNewInstances(true)
	router.KillIdleSandboxes(context.Background())
	router.KillAllNonCritical(context.Background())

	// WithRemote
	remoteRouter := router.WithRemote(NewRemoteSandbox("http://dummy", "", 1, nil))
	if remoteRouter == nil {
		t.Error("expected new router from WithRemote")
	}
}
