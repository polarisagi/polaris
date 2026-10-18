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
