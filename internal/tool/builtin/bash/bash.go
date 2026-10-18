package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/internal/tool/builtin/sandboxenv"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type bashArgs struct {
	Command string `json:"command"`
}

// MakeBashFn 创建 bash 工具执行函数。
//
// pol 携带信任档位的所有行为开关（ADR-0115 WP1 决策二）：
//   - pol.SandboxDisabled=true → 宿主直接执行（full 档）
//   - pol.ForceContainerSandbox=true → 覆盖 sandboxEnabled=false，强制容器（hardened 档）
//   - pol.HITLAutoApprove=true → 高风险命令自动放行+审计（full 档）
//   - pol.AllowedPathsOpen=true → bash 不受路径白名单限制（full 档）
//
// allowedPaths/sandboxEnabled/netPolicy/bwrapPath 来自 SandboxConfig，
// standard 档下行为与改造前逐字节等价（R1 向后兼容约束）。
func MakeBashFn(
	allowedPaths []string,
	sandboxEnabled bool,
	netPolicy protocol.SandboxNetworkPolicy,
	bwrapPath string,
	hitlGateway HITLGateway,
	pol policy.Policy,
) sandbox.InProcessFn {
	// 分级器只需构造一次：正则编译非零成本，工厂函数只在注册期调用一次，
	// 返回的闭包才是热路径（每次工具调用都会执行）。
	riskClassifier := classifier.NewDefaultClassifier()
	return func(ctx context.Context, input []byte) ([]byte, error) {
		// ADR-0097 决策五：项目会话追加项目工作目录为可访问根（会话级，不改进程级白名单）。
		paths := guard.ScopedPaths(ctx, allowedPaths)
		var args bashArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "bash: invalid args", err)
		}
		if args.Command == "" {
			return nil, apperr.New(apperr.CodeInternal, "bash: command is required")
		}

		verdict, err := evaluateBashRisk(ctx, args.Command, pol, riskClassifier, hitlGateway)
		if err != nil {
			return nil, err
		}

		workDir := ""
		// full 档 AllowedPathsOpen：不限路径，workDir 仍取第一个可用路径（通常是项目目录）
		if len(paths) > 0 {
			workDir = paths[0]
		}

		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// 按 Policy 决定实际沙箱行为
		effectiveSandbox := sandboxEnabled
		if pol.SandboxDisabled {
			// full 档：宿主直接执行，忽略配置的 sandboxEnabled
			effectiveSandbox = false
		} else if pol.ForceContainerSandbox {
			// hardened 档：强制容器，覆盖 sandboxEnabled=false
			effectiveSandbox = true
		}

		slog.Info("bash: executing command",
			"sandbox_enabled", effectiveSandbox,
			"network", netPolicy,
			"risk", verdict.Level.String(),
			"cmd", args.Command,
			"dir", workDir,
			"profile", pol.Profile)

		var outBytes []byte
		var execErr error
		var sandboxMethod string

		if effectiveSandbox {
			// V2 统一沙箱接口：env preset 在 Rust 侧由 CallerType=builtin 推导，
			// 凭据过滤由 Rust 侧 CREDENTIAL_STRIP 规则保证（无需 Go 侧 sandboxenv.BaseEnv() 过滤）。
			sandboxPaths := paths
			if pol.AllowedPathsOpen {
				// full 档不传路径白名单限制，宿主侧自由访问
				sandboxPaths = nil
			}
			sandboxCtx := protocol.SandboxContext{
				CallerType:    protocol.CallerBuiltin,
				Command:       args.Command,
				Workdir:       workDir,
				AllowedPaths:  sandboxPaths,
				NetworkPolicy: ToSandboxNetPolicy(netPolicy),
				BwrapPath:     bwrapPath, // Linux 用户自定义 bwrap 路径（空=自动查找）
				TimeoutMs:     30_000,
			}
			var setupErr error
			outBytes, execErr, sandboxMethod, setupErr = ExecInSandbox(execCtx, sandboxCtx)
			if setupErr != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "bash: sandbox wrap failed", setupErr)
			}
		} else {
			// 沙箱禁用：env 清理 + workDir + Linux namespace（最后防线）
			sandboxMethod = "disabled"
			outBytes, execErr = ExecWithoutSandbox(execCtx, args.Command, workDir, sandboxenv.BaseEnv())
		}

		result := map[string]any{
			"command":         args.Command,
			"output":          string(outBytes),
			"exit_code":       0,
			"sandbox_enabled": effectiveSandbox,
			"sandbox_method":  sandboxMethod,
			"network_policy":  netPolicy,
		}
		if execErr != nil {
			result["error"] = execErr.Error()
			var exitErr *exec.ExitError
			if errors.As(execErr, &exitErr) {
				result["exit_code"] = exitErr.ExitCode()
			} else {
				result["exit_code"] = -1
			}
		}
		return json.Marshal(result)
	}
}

func ExecInSandbox(_ context.Context, sandboxCtx protocol.SandboxContext) ([]byte, error, string, error) {
	resp, setupErr := toolsb.RustSandboxExecV2(sandboxCtx, sandboxCtx.TimeoutMs)
	if setupErr != nil {
		return nil, nil, "", setupErr //nolint:wrapcheck
	}
	var cmdErr error
	if resp.ExitCode != 0 {
		cmdErr = apperr.New(apperr.CodeInternal, fmt.Sprintf("exit status %d", resp.ExitCode))
	}
	return []byte(resp.Output), cmdErr, resp.SandboxMethod, nil
}

// ExecWithoutSandbox 在沙箱已被上游显式禁用（sandboxEnabled==false）时的最后防线执行路径。
// XR-10 豁免说明：这是 RunSandboxedArgv/ExecInSandbox 不可用时的降级分支本身（调用方已经
// 决定跳过沙箱），因此这里不能再次委托沙箱包装（会形成递归降级/无意义包装）；安全性由
// 调用方负责（env 清理 + workDir 限制 + Linux namespace 隔离，见调用点 bash.go:93 注释），
// 本函数只做裸执行。
func ExecWithoutSandbox(ctx context.Context, command, workDir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = workDir
	cmd.Env = env
	return cmd.CombinedOutput() //nolint:wrapcheck
}

func ToSandboxNetPolicy(p protocol.SandboxNetworkPolicy) string {
	if p == protocol.NetPolicyAllow {
		return protocol.NetPolicyAllow
	}
	return protocol.NetPolicyDeny
}

func evaluateBashRisk(ctx context.Context, cmd string, pol policy.Policy, riskClassifier *classifier.CommandRiskClassifier, hitlGateway HITLGateway) (classifier.Verdict, error) {
	verdict := riskClassifier.Classify(cmd)
	switch verdict.Level {
	case classifier.RiskDeny:
		slog.Error("bash: command DENIED by risk classifier",
			"cmd", cmd, "reason", verdict.Reason)
		return verdict, apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("bash: command denied: %s", verdict.Reason))
	case classifier.RiskHITL:
		// full 档：自动放行 + 记录审计日志（ADR-0115 WP1 行为矩阵）
		if pol.HITLAutoApprove {
			slog.Warn("bash: HITL auto-approved (full profile)",
				"cmd", cmd, "reason", verdict.Reason,
				"profile", pol.Profile)
			return verdict, nil
		}
		if hitlGateway == nil {
			return verdict, apperr.New(apperr.CodeForbidden, "bash: HITL gateway is nil, fail-closed for RiskHITL command")
		}
		slog.Warn("bash: command requires human approval (HITL), requesting approval",
			"cmd", cmd, "reason", verdict.Reason)
		if err := hitlGateway.RequestApproval(ctx, "bash", map[string]any{"command": cmd, "reason": verdict.Reason}); err != nil {
			return verdict, apperr.Wrap(apperr.CodeForbidden, "bash: HITL approval failed", err)
		}
	case classifier.RiskWarn:
		slog.Warn("bash: elevated-risk command executing",
			"cmd", cmd, "reason", verdict.Reason)
	}
	return verdict, nil
}
