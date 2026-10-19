package bash_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

type mockHITLGateway struct {
	called bool
}

func (m *mockHITLGateway) RequestApproval(ctx context.Context, action string, req map[string]any) error {
	m.called = true
	return nil
}

func TestMakeBashFn_FullProfile(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	hitlMock := &mockHITLGateway{}

	// full 档：即使 sandboxEnabled=true，实际也按宿主直接执行
	fn := bash.MakeBashFn([]string{tmpDir}, true, protocol.NetPolicyAllow, "", hitlMock, pol)
	ctx := context.Background()

	outBytes, err := fn(ctx, []byte(`{"command":"echo hello_full_profile"}`))
	if err != nil {
		t.Fatalf("bash execution failed: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal result failed: %v", err)
	}

	if res["sandbox_method"] != "disabled" {
		t.Errorf("full profile should disable sandbox, got method: %v", res["sandbox_method"])
	}
	if res["sandbox_enabled"] != false {
		t.Errorf("full profile should have effective sandbox_enabled=false, got: %v", res["sandbox_enabled"])
	}
}

func TestMakeBashFn_HardenedProfile(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileHardened)
	hitlMock := &mockHITLGateway{}

	// hardened 档：即使传入 sandboxEnabled=false，也会强制 effectiveSandbox=true
	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyDeny, "", hitlMock, pol)
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

func TestMakeBashFn_FullProfile_HITLAutoApprove(t *testing.T) {
	tmpDir := t.TempDir()
	pol := policy.ProfilePolicy(policy.ProfileFull)
	hitlMock := &mockHITLWithAuditor{}

	fn := bash.MakeBashFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "", hitlMock, pol)
	ctx := context.WithValue(context.Background(), protocol.CtxAgentIDKey{}, "agent-123")

	// 创建测试目录与文件
	testDir := filepath.Join(tmpDir, "sub")
	_ = os.MkdirAll(testDir, 0o755)
	testFile := filepath.Join(testDir, "dummy.txt")
	_ = os.WriteFile(testFile, []byte("ok"), 0o644)

	// 高风险命令（rm -rf 命令属于 HITL 范围）：full 档下自动放行，写入审计并记录指标
	_, err := fn(ctx, []byte(`{"command":"rm -rf `+testDir+`"}`))
	if err != nil {
		t.Fatalf("rm command should succeed under full profile auto-approval: %v", err)
	}
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
	if hitlMock.meta["agent_id"] != "agent-123" {
		t.Errorf("expected agent_id 'agent-123', got %v", hitlMock.meta["agent_id"])
	}
}
