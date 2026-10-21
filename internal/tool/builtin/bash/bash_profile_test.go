package bash_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

func TestMakeBashFn_FullProfile(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)

	// 显式授权：即使 sandboxEnabled=true，实际也按宿主直接执行（ADR-0121 决策三）
	fn := bash.MakeBashFn([]string{tmpDir}, true, protocol.NetPolicyAllow, "", pol)
	ctx := protocol.WithPreApproved(context.Background())

	outBytes, err := fn(ctx, []byte(`{"command":"echo hello_full_profile"}`))
	if err != nil {
		t.Fatalf("bash execution failed: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal result failed: %v", err)
	}

	if res["sandbox_method"] != "disabled" {
		t.Errorf("explicit grant should run on host, got method: %v", res["sandbox_method"])
	}
	if res["sandbox_enabled"] != false {
		t.Errorf("explicit grant should have effective sandbox_enabled=false, got: %v", res["sandbox_enabled"])
	}
}

func TestMakeBashFn_HardenedProfile(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileHardened)

	// hardened 档：即使传入 sandboxEnabled=false，也会强制 effectiveSandbox=true
	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyDeny, "", pol)
	ctx := context.Background()

	// 强制走容器沙箱：在无 bubblewrap/Seatbelt 环境或测试环境中 ExecInSandbox 尝试调用
	// 无论成功还是沙箱工具报错，effectiveSandbox 必须为 true（覆盖 false）
	outBytes, err := fn(ctx, []byte(`{"command":"echo hello"}`))
	// 在没有 Rust sandbox 动态库的环境下可能会报错，如果返回了结果则检查 sandbox_enabled
	if err == nil {
		var res map[string]any
		if err := json.Unmarshal(outBytes, &res); err == nil {
			if res["sandbox_enabled"] != true {
				t.Errorf("hardened profile should force effective sandbox_enabled=true, got: %v", res["sandbox_enabled"])
			}
		}
	}
}

// 无凭据时 full 档也不再宿主执行：trust_profile 不带来授权（ADR-0121 决策一）。
func TestMakeBashFn_FullProfile_NoGrantDoesNotRunOnHost(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	fn := bash.MakeBashFn([]string{tmpDir}, true, protocol.NetPolicyAllow, "", pol)
	outBytes, err := fn(context.Background(), []byte(`{"command":"echo x"}`))
	if err != nil {
		return // 测试环境无沙箱后端时 setup 失败也说明没有落到宿主
	}
	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err == nil && res["sandbox_method"] == "disabled" {
		t.Errorf("no grant must not run on host, got %v", res)
	}
}

// 显式授权的危险命令直接放行（Decide 已判定）；无凭据时由唯一的 shell 分析器 fail-closed 拒绝（ADR-0122 决策一）。
func TestMakeBashFn_GrantDecidesDangerousCommand(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileStandard)
	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", pol)

	testDir := filepath.Join(tmpDir, "sub")
	_ = os.MkdirAll(testDir, 0o755)
	_ = os.WriteFile(filepath.Join(testDir, "dummy.txt"), []byte("ok"), 0o644)

	outside := t.TempDir()
	if _, err := fn(context.Background(), []byte(`{"command":"rm -rf `+outside+`"}`)); err == nil {
		t.Fatal("no grant: rm outside workspace (dangerous) must be rejected")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("rejected command must not have run: %v", err)
	}

	ctx := protocol.WithPreApproved(context.Background())
	if _, err := fn(ctx, []byte(`{"command":"rm -rf `+testDir+`"}`)); err != nil {
		t.Fatalf("explicit grant: rm should succeed: %v", err)
	}
}

func TestMakeBashFn_HardenedProfile_BackgroundAndPersistentRejected(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileHardened)
	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyDeny, "", pol)
	ctx := context.Background()

	// 1. 后台任务模式在 hardened 档下被明确拒绝（R3-08）
	_, err := fn(ctx, []byte(`{"command":"echo test","background":true}`))
	if err == nil {
		t.Fatal("expected error for background job in hardened profile, got nil")
	}
	if !strings.Contains(err.Error(), "background jobs are disabled in hardened trust profile") {
		t.Errorf("unexpected error message: %v", err)
	}

	// 2. 持久终端模式在 hardened 档下被明确拒绝（R3-08）
	_, err = fn(ctx, []byte(`{"command":"echo test","session_id":"sess-hardened"}`))
	if err == nil {
		t.Fatal("expected error for persistent terminal in hardened profile, got nil")
	}
	if !strings.Contains(err.Error(), "persistent terminal is disabled in hardened trust profile") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// hardened 档即使显式授权也不在宿主执行（ADR-0121 决策一）。
func TestMakeBashFn_HardenedProfile_ExplicitGrantStillContainer(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileHardened)
	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyDeny, "", pol)
	ctx := protocol.WithPreApproved(context.Background())

	outBytes, err := fn(ctx, []byte(`{"command":"echo hello"}`))
	if err != nil {
		return // 无沙箱动态库的环境里 setup 失败也说明没有落到宿主
	}
	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err == nil && res["sandbox_enabled"] != true {
		t.Errorf("hardened must stay in container under explicit grant, got %v", res["sandbox_enabled"])
	}
}
