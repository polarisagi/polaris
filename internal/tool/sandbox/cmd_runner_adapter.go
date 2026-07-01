// Package sandbox — cmd_runner_adapter.go
//
// WrapBashCmdRunner 实现 internal/sandbox.CmdRunner 接口，
// 以 WrapBashCmd（Rust FFI 优先 → Go 降级）为执行后端。
//
// 调用链：ContainerSandbox（internal/sandbox）
//           → CmdRunner 接口
//           → WrapBashCmdRunner（此文件）
//           → WrapBashCmd → RustSandboxExec / wrapBashCmdGo
//
// 启动时由 cmd/polaris/boot_tools.go 注入到 NewContainerSandbox。

package sandbox

import (
	"context"
	"errors"
	"os/exec"

	isandbox "github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// WrapBashCmdRunner 实现 isandbox.CmdRunner，无状态，可安全并发使用。
type WrapBashCmdRunner struct{}

// NewWrapBashCmdRunner 构造默认实现。
func NewWrapBashCmdRunner() *WrapBashCmdRunner { return &WrapBashCmdRunner{} }

// RunCmd 执行命令并等待完成，返回合并输出（stdout+stderr）、退出码和沙箱方法。
func (r *WrapBashCmdRunner) RunCmd(ctx context.Context, cfg isandbox.CmdRunnerCfg) ([]byte, int, string, error) {
	natCfg := NativeSandboxCfg{
		Command:      cfg.Command,
		WorkDir:      cfg.WorkDir,
		AllowedPaths: cfg.AllowedPaths,
		Env:          cfg.Env,
		TimeoutMs:    cfg.TimeoutMs,
	}
	if cfg.NetworkBlock {
		natCfg.NetworkPolicy = NetworkBlock
	} else {
		natCfg.NetworkPolicy = NetworkAllow
	}

	goCmd, rustResp, goMethod, err := WrapBashCmd(ctx, natCfg)
	if err != nil {
		// WrapBashCmd 在 network_block 要求无法被真实隔离工具满足时会 fail-closed
		// 返回 error（CodeForbidden）——此处如实透传，不吞掉降级拒绝转成"内部错误"。
		return nil, -1, "", apperr.Wrap(apperr.CodeInternal, "cmd_runner: WrapBashCmd failed", err)
	}

	// Rust FFI 路径：命令已在 Rust 侧执行完毕，直接返回结果（sandbox_method 由 Rust 如实上报）。
	if rustResp != nil {
		return []byte(rustResp.Output), rustResp.ExitCode, rustResp.SandboxMethod, nil
	}

	// Go 降级路径：调用方（此处）负责运行 exec.Cmd。method 是 WrapBashCmd 探测到的
	// 真实隔离方式（"seatbelt"/"bwrap"/"wsl2"/"bare"），不再统一贴 "go_native" 标签——
	// 那个标签会掩盖"到底有没有真隔离"这个下游 native_os_sandbox.go/sandbox_impl.go
	// 本该校验却从未校验的关键事实。
	out, runErr := goCmd.CombinedOutput()
	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			// 非零退出码：命令本身失败，不视为 runner 错误，返回 exitCode 让调用方决策。
			exitCode = ee.ExitCode()
			// CombinedOutput 已合并 stderr 到 out，此处不额外追加。
			return out, exitCode, goMethod, nil
		}
		// exec 启动失败（如二进制不存在）：视为 runner 级别错误。
		return nil, -1, goMethod, apperr.Wrap(apperr.CodeInternal, "cmd_runner: exec failed", runErr)
	}
	return out, 0, goMethod, nil
}
