package run_command_test

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/run_command"
)

type mockHITLGateway struct{}

func (m *mockHITLGateway) RequestApproval(ctx context.Context, action string, req map[string]any) error {
	return nil
}

type mockHITLWithAuditor struct {
	called bool
	logged bool
	action string
	meta   map[string]any
}

func (m *mockHITLWithAuditor) RequestApproval(ctx context.Context, action string, req map[string]any) error {
	m.called = true
	return nil
}

func (m *mockHITLWithAuditor) Log(ctx context.Context, action string, meta map[string]any) error {
	m.logged = true
	m.action = action
	m.meta = meta
	return nil
}

func TestMakeRunCommandFn_FullProfile_NoWhitelist(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	hitlMock := &mockHITLGateway{}

	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", hitlMock, pol)
	ctx := context.Background()

	// "ls" 不在 run_command 默认的构建工具白名单中（默认只允许 go/cargo/npm 等）
	// full 档下白名单关闭，应正常执行
	out, err := fn(ctx, []byte(`{"command":"ls -la"}`))
	if err != nil {
		t.Fatalf("full profile should allow non-whitelisted commands: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestMakeRunCommandFn_StandardProfile_WhitelistEnforced(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileStandard)
	hitlMock := &mockHITLGateway{}

	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyDeny, "", hitlMock, pol)
	ctx := context.Background()

	// standard 档下 "ls" 必须被白名单拒绝
	_, err := fn(ctx, []byte(`{"command":"ls -la"}`))
	if err == nil {
		t.Fatal("standard profile should reject non-whitelisted command 'ls'")
	}
	if !strings.Contains(err.Error(), "not in whitelist") {
		t.Errorf("expected whitelist error, got: %v", err)
	}
}

func TestMakeRunCommandFn_FullProfile_HITLAutoApprove(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	hitlMock := &mockHITLWithAuditor{}

	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", hitlMock, pol)
	ctx := context.WithValue(context.Background(), protocol.CtxAgentIDKey{}, "agent-rc-1")

	// 高风险命令（pip install 属于 RiskHITL）：full 档下自动放行，写入审计并记录指标
	// 即使命令因未安装或网络失败，HITL 校验发生在执行前
	_, _ = fn(ctx, []byte(`{"command":"pip install something"}`))

	if hitlMock.called {
		t.Error("full profile should auto-approve without invoking RequestApproval")
	}
	if !hitlMock.logged {
		t.Fatal("full profile HITL auto-approve must write audit record (V9 / HE-1)")
	}
	if hitlMock.action != "hitl_auto_approved" {
		t.Errorf("expected action 'hitl_auto_approved', got %q", hitlMock.action)
	}
	if hitlMock.meta["checkpoint_type"] != "command_risk" {
		t.Errorf("expected checkpoint_type 'command_risk', got %v", hitlMock.meta["checkpoint_type"])
	}
	if hitlMock.meta["tool"] != "run_command" {
		t.Errorf("expected tool 'run_command', got %v", hitlMock.meta["tool"])
	}
	if hitlMock.meta["agent_id"] != "agent-rc-1" {
		t.Errorf("expected agent_id 'agent-rc-1', got %v", hitlMock.meta["agent_id"])
	}
}
