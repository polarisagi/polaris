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
	"github.com/polarisagi/polaris/internal/tool/builtin/sandboxenv"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type bashArgs struct {
	Command string `json:"command"`
}

func MakeBashFn(allowedPaths []string, sandboxEnabled bool, netPolicy protocol.SandboxNetworkPolicy, bwrapPath string) sandbox.InProcessFn {
	// 分级器只需构造一次：正则编译非零成本，工厂函数只在注册期调用一次，
	// 返回的闭包才是热路径（每次工具调用都会执行）。
	riskClassifier := classifier.NewDefaultClassifier()
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args bashArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "bash: invalid args", err)
		}
		if args.Command == "" {
			return nil, apperr.New(apperr.CodeInternal, "bash: command is required")
		}

		// ── 安全审核：CommandRiskClassifier ──────────────────────────────────
		// DENY → 直接拒绝，不执行。HITL → 当前 Phase1 记录日志 + 执行（Phase2 挂起等待审批）。
		// WARN → 强化审计日志 + 执行。SAFE → 直接执行。
		verdict := riskClassifier.Classify(args.Command)
		switch verdict.Level {
		case classifier.RiskDeny:
			slog.Error("bash: command DENIED by risk classifier",
				"cmd", args.Command, "reason", verdict.Reason)
			return nil, apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("bash: command denied: %s", verdict.Reason))
		case classifier.RiskHITL:
			// Phase1: 警告日志 + 继续执行（Phase2 将挂起等待 HITL 审批）
			slog.Warn("bash: command requires human approval (HITL) — executing in Phase1 mode",
				"cmd", args.Command, "reason", verdict.Reason)
		case classifier.RiskWarn:
			slog.Warn("bash: elevated-risk command executing",
				"cmd", args.Command, "reason", verdict.Reason)
		}

		workDir := ""
		if len(allowedPaths) > 0 {
			workDir = allowedPaths[0]
		}

		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		slog.Info("bash: executing command",
			"sandbox_enabled", sandboxEnabled,
			"network", netPolicy,
			"risk", verdict.Level.String(),
			"cmd", args.Command,
			"dir", workDir)

		var outBytes []byte
		var execErr error
		var sandboxMethod string

		if sandboxEnabled {
			// V2 统一沙箱接口：env preset 在 Rust 侧由 CallerType=builtin 推导，
			// 凭据过滤由 Rust 侧 CREDENTIAL_STRIP 规则保证（无需 Go 侧 sandboxenv.BaseEnv() 过滤）。
			sandboxCtx := protocol.SandboxContext{
				CallerType:    protocol.CallerBuiltin,
				Command:       args.Command,
				Workdir:       workDir,
				AllowedPaths:  allowedPaths,
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
			"sandbox_enabled": sandboxEnabled,
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
