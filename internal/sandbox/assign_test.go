package sandbox

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestAssignSandboxTier_TrustFloorExplicit(t *testing.T) {
	// 相同的工具属性，仅传参 trustTier 不同
	tool := types.Tool{
		Name:       "test-tool",
		Source:     types.ToolBuiltin, // 默认 Floor 是 InProcess
		Capability: types.CapReadOnly,
	}

	// 当 explicitly passed TrustTier 是 TrustUntrusted 时，Floor 是 Wasm
	got1, err1 := AssignSandboxTier(tool, types.TrustUntrusted, 1, "linux")
	if err1 != nil {
		t.Fatalf("unexpected error: %v", err1)
	}
	if got1 != types.SandboxWasm {
		t.Errorf("expected SandboxWasm, got %v", got1)
	}

	// 当 explicitly passed TrustTier 是 TrustSystem 时，Floor 是 InProcess
	got2, err2 := AssignSandboxTier(tool, types.TrustSystem, 1, "linux")
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if got2 != types.SandboxInProcess {
		t.Errorf("expected SandboxInProcess, got %v", got2)
	}
}
