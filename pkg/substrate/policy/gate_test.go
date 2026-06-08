package policy

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestGate_DenyByDefault(t *testing.T) {
	g := NewGate(nil)
	ctx := context.Background()
	// 无匹配规则 → deny
	allowed, err := g.IsAuthorized(ctx, "agent1", "unknown_action", "resource", nil)
	if err != nil || allowed {
		t.Fatalf("expected deny-by-default, got allowed=%v err=%v", allowed, err)
	}
}

func TestGate_ForbidOverridesPermit(t *testing.T) {
	g := NewGate(nil)
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
	g := NewGate(nil)
	ctx := context.Background()
	allowed, _ := g.IsAuthorized(ctx, "admin", "disable", "audit_log/events", nil)
	if allowed {
		t.Fatal("disabling audit_log must be forbidden")
	}
}

func TestGate_PrivilegedActionRequiresApproval(t *testing.T) {
	g := NewGate(nil)
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
	g := NewGate(nil)
	ctx := context.Background()
	allowed, err := g.IsAuthorized(ctx, "agent1", "read_local", "/tmp/file",
		map[string]any{"trust_level": 2})
	if err != nil || !allowed {
		t.Fatalf("read_local with trust>=1 should be permitted: allowed=%v err=%v", allowed, err)
	}
}

func TestGate_NetworkDialRequiresCapability(t *testing.T) {
	g := NewGate(nil)
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
	g := NewGate(nil)
	// TaintNone + TaintLow → ok
	if err := g.TaintEgressCheck(protocol.TaintNone, protocol.TaintLow); err != nil {
		t.Fatalf("low taint should pass egress: %v", err)
	}
	// TaintHigh → blocked
	if err := g.TaintEgressCheck(protocol.TaintHigh); err == nil {
		t.Fatal("high taint must be blocked at egress")
	}
	// TaintMedium + TaintHigh → blocked（max 传播）
	if err := g.TaintEgressCheck(protocol.TaintMedium, protocol.TaintHigh); err == nil {
		t.Fatal("mixed taint with high must be blocked")
	}
}

func TestGate_InvalidRequest(t *testing.T) {
	g := NewGate(nil)
	ctx := context.Background()
	_, err := g.IsAuthorized(ctx, "", "read", "res", nil)
	if err == nil {
		t.Fatal("empty principal must return error")
	}
}

func TestGate_KillSwitchTriggeredOnConsecutiveFailures(t *testing.T) {
	triggered := false
	g := NewGate(func() { triggered = true })
	ctx := context.Background()
	// 连续发送空 principal 触发 failure 计数
	for i := 0; i < 10; i++ {
		g.IsAuthorized(ctx, "", "action", "res", nil)
	}
	if !triggered {
		t.Fatal("KillSwitch must be triggered after 10 consecutive failures")
	}
}
