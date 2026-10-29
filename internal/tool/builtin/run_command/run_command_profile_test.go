package run_command_test

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/run_command"
)

func TestMakeRunCommandFn_ExplicitGrant_NoWhitelist(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)

	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", pol)
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

	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyDeny, "", pol)
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

// 无凭据的危险命令被唯一的 shell 分析器拒绝；显式授权的不在工具内再分级（ADR-0122 决策一）。
func TestMakeRunCommandFn_UngrantedDangerousRejected(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	fn := run_command.MakeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", pol)

	// 白名单内的前缀（go）+ 管道进 shell：分析器判灾难性，无凭据必须拒绝
	_, err := fn(context.Background(), []byte(`{"command":"go env | sh"}`))
	if err == nil || !strings.Contains(err.Error(), "requires permission approval") {
		t.Fatalf("ungranted dangerous command must be rejected, got %v", err)
	}
}
