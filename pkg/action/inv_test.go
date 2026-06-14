package action

import (
	"errors"
	"testing"
)

// Test_inv_M7_04_DelegationChainMaxDepth 验证委托链深度不超过 3。
// inv_M7_04: ValidateDelegation(parentDepth >= 3) → ErrMaxDelegationDepth。
func Test_inv_M7_04_DelegationChainMaxDepth(t *testing.T) {
	dummyToken, _ := NewJITToken("agent-root", "session-root", []TokenOperation{{ToolName: "bash"}}, 0)

	// parentDepth=3 → 直接被 ValidateDelegation 检查拦截
	_, err := ValidateDelegation(dummyToken, 3, "agent-a", "session-1", nil)
	if !errors.Is(err, ErrMaxDelegationDepth) {
		t.Errorf("inv_M7_04: parentDepth=3 got %v, want ErrMaxDelegationDepth", err)
	}

	// parentDepth=5 → 超出上限，同样拦截
	_, err = ValidateDelegation(dummyToken, 5, "agent-a", "session-1", nil)
	if !errors.Is(err, ErrMaxDelegationDepth) {
		t.Errorf("inv_M7_04: parentDepth=5 got %v, want ErrMaxDelegationDepth", err)
	}

	// parentDepth=100 → 边界极限
	_, err = ValidateDelegation(dummyToken, 100, "agent-a", "session-1", nil)
	if !errors.Is(err, ErrMaxDelegationDepth) {
		t.Errorf("inv_M7_04: parentDepth=100 got %v, want ErrMaxDelegationDepth", err)
	}
}

// Test_inv_M7_04_AllowedDepthBelowThreshold 验证 parentDepth=0 时能正常签发令牌。
// ValidateDelegation(0,...) → NewJITToken(depth=1) → 低于阈值，Mint 成功。
func Test_inv_M7_04_AllowedDepthBelowThreshold(t *testing.T) {
	dummyToken, _ := NewJITToken("agent-root", "session-root", []TokenOperation{{ToolName: "bash"}}, 0)
	tok, err := ValidateDelegation(dummyToken, 0, "agent-root", "session-root", []TokenOperation{
		{ToolName: "bash", MaxCalls: 1},
	})
	if errors.Is(err, ErrMaxDelegationDepth) {
		t.Errorf("inv_M7_04: parentDepth=0 should be allowed, got ErrMaxDelegationDepth")
	}
	if err == nil && tok == nil {
		t.Error("inv_M7_04: parentDepth=0 returned nil token without error")
	}
}

// Test_inv_M7_04_BoundaryAtExactlyDepthThree 验证边界值精确为 3。
// parentDepth=2 在 ValidateDelegation 内部通过检查，但 NewJITToken(depth=3) 仍触发拒绝。
// 这验证了链式实现的一致性：两层检查共同保证深度上限。
func Test_inv_M7_04_BoundaryAtExactlyDepthThree(t *testing.T) {
	dummyToken, _ := NewJITToken("agent-root", "session-root", []TokenOperation{{ToolName: "bash"}}, 0)
	// 确认深度=2 也会因 NewJITToken(depth=3) 而被拒绝
	_, err := ValidateDelegation(dummyToken, 2, "agent-b", "session-2", nil)
	if !errors.Is(err, ErrMaxDelegationDepth) {
		t.Errorf("inv_M7_04: parentDepth=2 (chain depth=3) got %v, want ErrMaxDelegationDepth", err)
	}
}
