package sandbox

import (
	"context"
	"time"

	isandbox "github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const boundedCmdOutputLimit = 64 * 1024

// BoundedCmdRunner 通过 Rust 原生沙箱运行命令，并分别限制 stdout 与 stderr。
type BoundedCmdRunner struct {
	wrapper isandbox.ArgvWrapper
}

// NewBoundedCmdRunner 构造代码补丁评测使用的生产执行器。
func NewBoundedCmdRunner() *BoundedCmdRunner {
	return &BoundedCmdRunner{wrapper: NewRustArgvWrapper()}
}

// RunCmd 在物理沙箱中执行命令，沙箱不可用时拒绝启动。
func (r *BoundedCmdRunner) RunCmd(ctx context.Context, cfg isandbox.CmdRunnerCfg) ([]byte, int, string, error) {
	if r == nil || r.wrapper == nil {
		return nil, -1, "", apperr.New(apperr.CodeForbidden, "bounded cmd runner: sandbox wrapper unavailable")
	}
	if !cfg.NetworkBlock {
		return nil, -1, "", apperr.New(apperr.CodeForbidden, "bounded cmd runner: network blocking is required")
	}
	deny, denyWrite := policy.SandboxDenyPaths()
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	result, err := isandbox.RunStdio(ctx, r.wrapper, isandbox.StdioRequest{
		CallerType:      cfg.CallerType,
		Command:         cfg.Command,
		WorkDir:         cfg.WorkDir,
		AllowedPaths:    cfg.AllowedPaths,
		DenyPaths:       mergePaths(deny, cfg.DenyPaths),
		DenyWritePaths:  mergePaths(denyWrite, cfg.DenyWritePaths),
		Env:             cfg.Env,
		AllowNet:        false,
		Timeout:         timeout,
		MaxOutput:       boundedCmdOutputLimit,
		RequirePhysical: true,
	})
	output := make([]byte, 0, len(result.Stdout)+len(result.Stderr))
	output = append(output, result.Stdout...)
	output = append(output, result.Stderr...)
	if err != nil {
		return output, -1, result.Method, apperr.Wrap(apperr.CodeOf(err), "bounded cmd runner: run in physical sandbox", err)
	}
	if result.TimedOut {
		return output, -1, result.Method, apperr.New(apperr.CodeTimeout, "bounded cmd runner: command timed out")
	}
	return output, result.ExitCode, result.Method, nil
}

var _ isandbox.CmdRunner = (*BoundedCmdRunner)(nil)
