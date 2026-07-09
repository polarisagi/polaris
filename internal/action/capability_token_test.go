package action

import (
	"testing"
)

func TestValidateDelegation_SandboxTier(t *testing.T) {
	// 创建一个父 token，SandboxTier = 2
	ops := []TokenOperation{{ToolName: "fetch_url"}}
	parentTok, err := NewJITToken("agent-A", "session-1", ops, 0, 2)
	if err != nil {
		t.Fatalf("failed to mint parent token: %v", err)
	}

	// 1. 同级委托 (2 -> 2)
	subTok, err := ValidateDelegation(parentTok, 1, "agent-B", "session-1", ops, 2)
	if err != nil {
		t.Errorf("expected success for same tier delegation, got: %v", err)
	}
	if subTok == nil || subTok.Claims.SandboxTier != 2 {
		t.Errorf("sub token tier mismatch")
	}

	// 2. 提升隔离度委托 (2 -> 3) (更安全，应允许)
	subTok2, err := ValidateDelegation(parentTok, 1, "agent-B", "session-1", ops, 3)
	if err != nil {
		t.Errorf("expected success for stronger isolation delegation, got: %v", err)
	}
	if subTok2 == nil || subTok2.Claims.SandboxTier != 3 {
		t.Errorf("sub token tier mismatch")
	}

	// 3. 降低隔离度委托 (2 -> 1) - 越权（沙箱逃逸），应该失败
	_, err = ValidateDelegation(parentTok, 1, "agent-B", "session-1", ops, 1)
	if err != ErrSandboxTierEscalation {
		t.Errorf("expected ErrSandboxTierEscalation for isolation downgrade, got: %v", err)
	}
}
