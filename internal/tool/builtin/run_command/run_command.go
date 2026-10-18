package run_command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/internal/tool/builtin/sandboxenv"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type runCommandArgs struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir"`
	TimeoutS   int    `json:"timeout_s"`
}

// MakeRunCommandFn 创建 run_command 工具执行函数。
//
// pol 携带信任档位的所有行为开关（ADR-0115 WP1 决策二）：
//   - pol.RunCmdWhitelistEnabled=false → full 档关闭命令前缀白名单
//   - pol.SandboxDisabled=true → 宿主直接执行（full 档）
//   - pol.ForceContainerSandbox=true → 强制容器（hardened 档）
//   - pol.HITLAutoApprove=true → 高风险命令自动放行（full 档）
//
// standard 档行为与改造前逐字节等价（R1 向后兼容约束）。
func MakeRunCommandFn(
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
		args, workDir, timeout, err := parseRunCommandArgs(input, paths, pol)
		if err != nil {
			return nil, err
		}

		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// ── 安全审核：CommandRiskClassifier ──────────────────────────────────
		verdict := riskClassifier.Classify(args.Command)
		switch verdict.Level {
		case classifier.RiskDeny:
			slog.Error("run_command: command DENIED by risk classifier",
				"cmd", args.Command, "reason", verdict.Reason)
			return nil, apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("run_command: command denied: %s", verdict.Reason))
		case classifier.RiskHITL:
			// full 档：自动放行 + 审计（ADR-0115 WP1 行为矩阵）
			if pol.HITLAutoApprove {
				slog.Warn("run_command: HITL auto-approved (full profile)",
					"cmd", args.Command, "reason", verdict.Reason,
					"profile", pol.Profile)
			} else {
				if hitlGateway == nil {
					return nil, apperr.New(apperr.CodeForbidden, "run_command: HITL gateway is nil, fail-closed for RiskHITL command")
				}
				slog.Warn("run_command: command requires human approval (HITL), requesting approval",
					"cmd", args.Command, "reason", verdict.Reason)
				if err := hitlGateway.RequestApproval(ctx, "run_command", map[string]any{"command": args.Command, "reason": verdict.Reason}); err != nil {
					return nil, apperr.Wrap(apperr.CodeForbidden, "run_command: HITL approval failed", err)
				}
			}
		case classifier.RiskWarn:
			slog.Warn("run_command: elevated-risk command executing",
				"cmd", args.Command, "reason", verdict.Reason)
		}

		slog.Info("run_command: executing",
			"sandbox_enabled", sandboxEnabled,
			"network", netPolicy,
			"risk", verdict.Level.String(),
			"cmd", args.Command,
			"dir", workDir,
			"profile", pol.Profile)

		env := append(sandboxenv.BaseEnv(), "GOCACHE=/tmp/gocache", "CARGO_HOME=/tmp/cargo", "npm_config_cache=/tmp/npm")

		var outBytes []byte
		var execErr error
		var sandboxMethod string

		// 按 Policy 决定实际沙箱行为（与 bash.go 保持一致逻辑）
		effectiveSandbox := sandboxEnabled
		if pol.SandboxDisabled {
			effectiveSandbox = false
		} else if pol.ForceContainerSandbox {
			effectiveSandbox = true
		}

		if effectiveSandbox {
			// V2 统一沙箱接口：构建工具额外需要 GOCACHE/CARGO_HOME/npm_config_cache，
			// 通过 EnvExtra 显式注入（Rust 侧 credential 过滤后追加）。
			sandboxPaths := paths
			if pol.AllowedPathsOpen {
				sandboxPaths = nil
			}
			sandboxCtx := protocol.SandboxContext{
				CallerType:    protocol.CallerBuiltin,
				Command:       args.Command,
				Workdir:       workDir,
				AllowedPaths:  sandboxPaths,
				NetworkPolicy: bash.ToSandboxNetPolicy(netPolicy), // 构建工具通常需要网络（下载依赖），由上层配置控制
				EnvExtra:      []string{"GOCACHE=/tmp/gocache", "CARGO_HOME=/tmp/cargo", "npm_config_cache=/tmp/npm"},
				BwrapPath:     bwrapPath, // Linux 用户自定义 bwrap 路径（空=自动查找）
				TimeoutMs:     uint64(timeout.Milliseconds()),
			}
			var setupErr error
			outBytes, execErr, sandboxMethod, setupErr = bash.ExecInSandbox(execCtx, sandboxCtx)
			if setupErr != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "run_command: sandbox wrap failed", setupErr)
			}
		} else {
			sandboxMethod = "disabled"
			outBytes, execErr = bash.ExecWithoutSandbox(execCtx, args.Command, workDir, env)
		}

		result := map[string]any{
			"command":        args.Command,
			"output":         string(outBytes),
			"exit_code":      0,
			"sandbox_method": sandboxMethod,
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

func parseRunCommandArgs(input []byte, allowedPaths []string, pol policy.Policy) (*runCommandArgs, string, time.Duration, error) {
	var args runCommandArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, "", 0, apperr.Wrap(apperr.CodeInternal, "run_command: invalid args", err)
	}
	if args.Command == "" {
		return nil, "", 0, apperr.New(apperr.CodeInternal, "run_command: command is required")
	}

	// 命令前缀白名单（构建工具，不含 bash/sh 等 shell 解释器）
	// full 档 RunCmdWhitelistEnabled=false：关闭白名单，宿主直接执行任意命令。
	if pol.RunCmdWhitelistEnabled {
		cmdPrefix := strings.SplitN(strings.TrimSpace(args.Command), " ", 2)[0]
		allowedCmds := map[string]bool{
			"go": true, "cargo": true, "npm": true, "yarn": true, "pnpm": true,
			"make": true, "pytest": true, "tsc": true, "python": true, "python3": true,
			"pip": true, "pip3": true, "node": true, "deno": true, "bun": true,
		}
		if !allowedCmds[cmdPrefix] {
			return nil, "", 0, apperr.New(apperr.CodeForbidden, fmt.Sprintf("run_command: command %q not in whitelist", cmdPrefix))
		}
	}

	workDir := args.WorkingDir
	if workDir == "" && len(allowedPaths) > 0 {
		workDir = allowedPaths[0]
	}
	if workDir != "" && !pol.AllowedPathsOpen {
		if err := guard.CheckAllowedPath(workDir, allowedPaths); err != nil {
			return nil, "", 0, apperr.Wrap(apperr.CodeInternal, "makeRunCommandFn", err)
		}
	}

	timeout := time.Duration(args.TimeoutS) * time.Second
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 30 * time.Second
	}

	return &args, workDir, timeout, nil
}
