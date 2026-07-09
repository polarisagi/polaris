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

func MakeRunCommandFn(allowedPaths []string, sandboxEnabled bool, netPolicy protocol.SandboxNetworkPolicy, bwrapPath string) sandbox.InProcessFn {
	// 分级器只需构造一次：正则编译非零成本，工厂函数只在注册期调用一次，
	// 返回的闭包才是热路径（每次工具调用都会执行）。
	riskClassifier := classifier.NewDefaultClassifier()
	return func(ctx context.Context, input []byte) ([]byte, error) {
		args, workDir, timeout, err := parseRunCommandArgs(input, allowedPaths)
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
			slog.Warn("run_command: command requires human approval (HITL) — executing in Phase1 mode",
				"cmd", args.Command, "reason", verdict.Reason)
		case classifier.RiskWarn:
			slog.Warn("run_command: elevated-risk command executing",
				"cmd", args.Command, "reason", verdict.Reason)
		}

		slog.Info("run_command: executing",
			"sandbox_enabled", sandboxEnabled,
			"network", netPolicy,
			"risk", verdict.Level.String(),
			"cmd", args.Command,
			"dir", workDir)

		env := append(sandboxenv.BaseEnv(), "GOCACHE=/tmp/gocache", "CARGO_HOME=/tmp/cargo", "npm_config_cache=/tmp/npm")

		var outBytes []byte
		var execErr error
		var sandboxMethod string

		if sandboxEnabled {
			// V2 统一沙箱接口：构建工具额外需要 GOCACHE/CARGO_HOME/npm_config_cache，
			// 通过 EnvExtra 显式注入（Rust 侧 credential 过滤后追加）。
			sandboxCtx := protocol.SandboxContext{
				CallerType:    protocol.CallerBuiltin,
				Command:       args.Command,
				Workdir:       workDir,
				AllowedPaths:  allowedPaths,
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

func parseRunCommandArgs(input []byte, allowedPaths []string) (*runCommandArgs, string, time.Duration, error) {
	var args runCommandArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, "", 0, apperr.Wrap(apperr.CodeInternal, "run_command: invalid args", err)
	}
	if args.Command == "" {
		return nil, "", 0, apperr.New(apperr.CodeInternal, "run_command: command is required")
	}

	// 命令前缀白名单（构建工具，不含 bash/sh 等 shell 解释器）
	cmdPrefix := strings.SplitN(strings.TrimSpace(args.Command), " ", 2)[0]
	allowedCmds := map[string]bool{
		"go": true, "cargo": true, "npm": true, "yarn": true, "pnpm": true,
		"make": true, "pytest": true, "tsc": true, "python": true, "python3": true,
		"pip": true, "pip3": true, "node": true, "deno": true, "bun": true,
	}
	if !allowedCmds[cmdPrefix] {
		return nil, "", 0, apperr.New(apperr.CodeForbidden, fmt.Sprintf("run_command: command %q not in whitelist", cmdPrefix))
	}

	workDir := args.WorkingDir
	if workDir == "" && len(allowedPaths) > 0 {
		workDir = allowedPaths[0]
	}
	if workDir != "" {
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
