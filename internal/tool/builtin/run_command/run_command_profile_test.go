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

type mockHITLCounter struct{ called bool }

func (m *mockHITLCounter) RequestApproval(ctx context.Context, action string, req map[string]any) error {
	m.called = true
	return nil
}

func TestMakeRunCommandFn_ExplicitGrant_NoWhitelist(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	hitlMock := &mockHITLGateway{}

	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", hitlMock, pol)
	ctx := protocol.WithPreApproved(context.Background())

	// "ls" 不在 run_command 默认的构建工具白名单中（默认只允许 go/cargo/npm 等）
	// 有授权凭据时白名单不生效（ADR-0121 决策三），应正常执行
	out, err := fn(ctx, []byte(`{"command":"ls -la"}`))
	if err != nil {
		t.Fatalf("granted call should allow non-whitelisted commands: %v", err)
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

// 显式授权的高风险命令不再触发工具内 HITL。
func TestMakeRunCommandFn_ExplicitGrant_SkipsInToolHITL(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileStandard)
	hitlMock := &mockHITLCounter{}

	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", hitlMock, pol)
	ctx := protocol.WithPreApproved(context.Background())

	// pip install 属于 RiskHITL；即使命令因未安装或网络失败，HITL 校验发生在执行前
	_, _ = fn(ctx, []byte(`{"command":"pip install something"}`))

	if hitlMock.called {
		t.Error("explicit grant must not invoke in-tool RequestApproval")
	}
}
