package policy

import (
	"strings"
	"testing"
)

func TestCedarEngine_EmptyPolicy_Deny(t *testing.T) {
	engine := NewCedarEngine()
	// 重置策略为空
	if err := engine.SyncPolicies("// empty"); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	t.Cleanup(func() { engine.SyncPolicies("// empty") })

	allowed, reason, err := engine.Evaluate(`Agent::"agent-1"`, `Action::"infer"`, `Resource::"llm_api"`, map[string]any{"trust_level": 3}, 100)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}

	if allowed {
		t.Error("Empty policy should deny by default")
	}
	if reason != "deny" {
		t.Errorf("Expected reason 'deny', got: %s", reason)
	}
}

func TestCedarEngine_PermitPolicy_Allow(t *testing.T) {
	engine := NewCedarEngine()
	policies := `
		permit(
			principal,
			action == Action::"infer",
			resource
		) when {
			context.trust_level >= 1
		};
	`
	if err := engine.SyncPolicies(policies); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	t.Cleanup(func() { engine.SyncPolicies("// empty") })

	allowed, reason, err := engine.Evaluate(`Agent::"agent-1"`, `Action::"infer"`, `Resource::"llm_api"`, map[string]any{"trust_level": 3}, 100)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}

	if !allowed {
		t.Error("Policy should allow the request")
	}
	if reason != "allow" {
		t.Errorf("Expected reason 'allow', got: %s", reason)
	}
}

func TestCedarEngine_InvalidSyntax(t *testing.T) {
	engine := NewCedarEngine()
	t.Cleanup(func() { engine.SyncPolicies("// empty") })
	err := engine.SyncPolicies(`permit(invalid syntax);`)
	if err == nil {
		t.Error("Expected error on invalid policy syntax")
	} else if !strings.Contains(err.Error(), "policy parse error") {
		t.Errorf("Expected parse error, got: %v", err)
	}
}
