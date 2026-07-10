package policy

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestGate_DenyByDefault(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()
	// 无匹配规则 → deny
	allowed, err := g.IsAuthorized(ctx, "agent1", "unknown_action", "resource", nil)
	if err != nil || allowed {
		t.Fatalf("expected deny-by-default, got allowed=%v err=%v", allowed, err)
	}
}

func TestGate_ForbidOverridesPermit(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	// 同时添加 forbid 和 permit 匹配 "test_action" → forbid 赢
	g.AddForbidRule(ForbidRule{
		Name:   "test_forbid",
		Reason: "test",
		MatchFn: func(_, action, _ string, _ map[string]any) bool {
			return action == "test_action"
		},
	})
	g.AddPermitRule(PermitRule{
		Name: "test_permit",
		MatchFn: func(_, action, _ string, _ map[string]any) bool {
			return action == "test_action"
		},
	})
	ctx := context.Background()
	allowed, _ := g.IsAuthorized(ctx, "agent1", "test_action", "r", nil)
	if allowed {
		t.Fatal("forbid must override permit")
	}
}

func TestGate_AuditLogAlwaysOn(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()
	allowed, _ := g.IsAuthorized(ctx, "admin", "disable", "audit_log/events", nil)
	if allowed {
		t.Fatal("disabling audit_log must be forbidden")
	}
}

func TestGate_PrivilegedActionRequiresApproval(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()
	// 无 approval → forbid
	allowed, _ := g.IsAuthorized(ctx, "agent", "delete_data", "db/users", nil)
	if allowed {
		t.Fatal("delete_data without approval must be denied")
	}
	// 有 approval → 通过 forbid（但无 permit 规则 → deny-by-default）
	allowed, _ = g.IsAuthorized(ctx, "agent", "delete_data", "db/users",
		map[string]any{"approval_status": "approved"})
	// deny-by-default（无 permit 规则覆盖此 action）
	if allowed {
		t.Fatal("delete_data with approval still denied by default (no permit rule)")
	}
}

func TestGate_ReadLocalPermitted(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()
	allowed, err := g.IsAuthorized(ctx, "agent1", "read_local", "/tmp/file",
		map[string]any{"trust_level": 2})
	if err != nil || !allowed {
		t.Fatalf("read_local with trust>=1 should be permitted: allowed=%v err=%v", allowed, err)
	}
}

func TestGate_NetworkDialRequiresCapability(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()
	// 无 capability token → deny
	allowed, _ := g.IsAuthorized(ctx, "agent1", "network_dial", "example.com:443",
		map[string]any{"trust_level": 3})
	if allowed {
		t.Fatal("network_dial without capability token must be denied")
	}
	// 有 capability token + trust>=3 → permit
	allowed, _ = g.IsAuthorized(ctx, "agent1", "network_dial", "example.com:443",
		map[string]any{"trust_level": 3, "capability_token_valid": true})
	if !allowed {
		t.Fatal("network_dial with capability token and trust>=3 should be permitted")
	}
}

func TestGate_TaintEgressCheck(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	// TaintNone + TaintLow → ok
	if err := g.TaintEgressCheck(types.TaintNone, types.TaintLow); err != nil {
		t.Fatalf("low taint should pass egress: %v", err)
	}
	// TaintHigh → blocked
	if err := g.TaintEgressCheck(types.TaintHigh); err == nil {
		t.Fatal("high taint must be blocked at egress")
	}
	// TaintMedium + TaintHigh → blocked（max 传播）
	if err := g.TaintEgressCheck(types.TaintMedium, types.TaintHigh); err == nil {
		t.Fatal("mixed taint with high must be blocked")
	}
}

func TestGate_InvalidRequest(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()
	_, err := g.IsAuthorized(ctx, "", "read", "res", nil)
	if err == nil {
		t.Fatal("empty principal must return error")
	}
}

func TestGate_KillSwitchTriggeredOnConsecutiveFailures(t *testing.T) {
	triggered := false
	g := NewGate(func() { triggered = true }).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()
	// 连续发送空 principal 触发 failure 计数
	for i := 0; i < 10; i++ {
		g.IsAuthorized(ctx, "", "action", "res", nil)
	}
	if !triggered {
		t.Fatal("KillSwitch must be triggered after 10 consecutive failures")
	}
}

func TestGoFallbackPermits_SixActions(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()

	// tool_execute
	allow, _ := g.IsAuthorized(ctx, "agent", "tool_execute", "res", map[string]any{"trust_tier": 3})
	if !allow {
		t.Error("tool_execute with trust_tier>=3 should be allowed")
	}

	// process_spawn
	allow, _ = g.IsAuthorized(ctx, "mcp_mgr", "process_spawn", "res", map[string]any{"trust_tier": 3})
	if !allow {
		t.Error("process_spawn with trust_tier>=3 should be allowed")
	}

	// script_execute
	allow, _ = g.IsAuthorized(ctx, "agent", "script_execute", "res", map[string]any{"trust_tier": 1})
	if !allow {
		t.Error("script_execute with trust_tier>=1 should be allowed")
	}

	// hook_execute
	allow, _ = g.IsAuthorized(ctx, "agent", "hook_execute", "res", nil)
	if !allow {
		t.Error("hook_execute should be allowed")
	}

	// browser_automate
	allow, _ = g.IsAuthorized(ctx, "agent", "browser_automate", "lam", map[string]any{"allow_net": true})
	if !allow {
		t.Error("browser_automate lam allow_net=true should be allowed")
	}
}

func TestGate_NoGoroutineLeak(t *testing.T) {
	g := NewGate(nil).WithEvalTimeout(2 * time.Second)
	ctx := context.Background()

	initialCount := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		g.IsAuthorized(ctx, "agent", "tool_execute", "res", nil)
	}
	finalCount := runtime.NumGoroutine()

	// 确保没有为每次调用产生 goroutine 泄漏
	if finalCount > initialCount+10 { // 留出 runtime 自身波动余量
		t.Fatalf("goroutine leak detected: initial %d, final %d", initialCount, finalCount)
	}
}

func TestGate_CedarEnforceDeny(t *testing.T) {
	g := NewGate(nil).WithCedarEnforceMode(CedarEnforceDeny)
	err := g.SyncCedarPolicies(`permit(principal, action, resource) when { principal == Principal::"admin" };`)
	if err != nil {
		t.Skip("Cedar FFI not available, skipping test")
	}

	// Add a Go permit rule for "guest"
	g.AddPermitRule(PermitRule{
		Name: "test_permit_guest",
		MatchFn: func(principal, action, _ string, _ map[string]any) bool {
			return principal == "guest"
		},
	})

	ctx := context.Background()

	// In Shadow mode, this would be allowed because Go rules permit it.
	// In EnforceDeny mode, Cedar deny should override Go rules and return false immediately.
	allowed, err := g.IsAuthorized(ctx, "guest", "read", "data", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("expected CedarEnforceDeny to propagate deny and override Go permit rules")
	}
}
