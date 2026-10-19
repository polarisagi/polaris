package bash_test

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
)

// TestR2_02_SandboxEscape_BackgroundAndPersistent 覆盖 3 个档位 × {后台模式, 持久终端} 共 6 个用例：
// 断言 standard 和 hardened 档下宿主机执行调用为 0，防止逃出沙箱。
func TestR2_02_SandboxEscape_BackgroundAndPersistent(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	profiles := []struct {
		name    string
		profile policy.TrustProfile
	}{
		{"full", policy.ProfileFull},
		{"standard", policy.ProfileStandard},
		{"hardened", policy.ProfileHardened},
	}

	for _, p := range profiles {
		pol := policy.ProfilePolicy(p.profile)

		// 1. 后台模式 (Background: true)
		t.Run(p.name+"_background", func(t *testing.T) {
			jm := bash.GetDefaultJobManager()
			jobs0, _ := jm.ListJobs("")
			initialJobs := len(jobs0)

			fn := bash.MakeBashFn([]string{tmpDir}, true, protocol.NetPolicyAllow, "", &mockHITLGateway{}, pol)
			outBytes, err := fn(ctx, []byte(`{"command":"sleep 1","background":true}`))

			jobs1, _ := jm.ListJobs("")
			jobsAfter := len(jobs1)
			hostJobsStarted := jobsAfter - initialJobs

			switch p.profile {
			case policy.ProfileFull:
				if err != nil {
					t.Fatalf("full 档后台模式期望成功执行，实际错误: %v", err)
				}
				if hostJobsStarted != 1 {
					t.Fatalf("full 档期望在宿主机启动 1 个后台任务，实际新增: %d", hostJobsStarted)
				}
				_ = outBytes
			case policy.ProfileStandard:
				if err == nil {
					t.Fatalf("standard 档必须拒绝无沙箱的后台模式，实际未报错且在宿主执行")
				}
				if hostJobsStarted != 0 {
					t.Fatalf("standard 档宿主机后台执行次数必须为 0，实际新增: %d", hostJobsStarted)
				}
				if !strings.Contains(err.Error(), "background jobs require full trust profile") {
					t.Fatalf("standard 档错误信息不符合规约: %v", err)
				}
			case policy.ProfileHardened:
				if err == nil {
					t.Fatalf("hardened 档必须拒绝后台模式，实际未报错且在宿主执行")
				}
				if hostJobsStarted != 0 {
					t.Fatalf("hardened 档宿主机后台执行次数必须为 0，实际新增: %d", hostJobsStarted)
				}
				if !strings.Contains(err.Error(), "disabled in hardened") {
					t.Fatalf("hardened 档错误信息不符合规约: %v", err)
				}
			}
		})

		// 2. 持久终端模式 (session_id != "")
		t.Run(p.name+"_persistent", func(t *testing.T) {
			fn := bash.MakeBashFn([]string{tmpDir}, true, protocol.NetPolicyDeny, "", &mockHITLGateway{}, pol)
			_, err := fn(ctx, []byte(`{"command":"echo test","session_id":"test-sess"}`))

			switch p.profile {
			case policy.ProfileFull:
				// full 档下若没有配置持久沙箱后端，允许返回未配置错误，但不应是禁止档位错误
				if err != nil && strings.Contains(err.Error(), "disabled in hardened") {
					t.Fatalf("full 档不应报 hardened 错误: %v", err)
				}
			case policy.ProfileStandard:
				// standard 档下若无沙箱环境，必须拒绝或报沙箱不可用，绝不能回退到宿主机
				if err == nil {
					t.Fatalf("standard 档无可用持久沙箱时不能放行到宿主直接执行")
				}
			case policy.ProfileHardened:
				// hardened 档必须显式拒绝或限制持久终端
				if err == nil {
					t.Fatalf("hardened 档无安全沙箱后端时不能直接放行")
				}
				if !strings.Contains(err.Error(), "disabled in hardened") {
					t.Fatalf("hardened 档期望返回明确禁用错误，实际: %v", err)
				}
			}
		})
	}
}
