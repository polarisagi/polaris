// Package sandbox — cmd_runner_adapter.go
//
// WrapBashCmdRunner 实现 internal/sandbox.CmdRunner 接口，
// 以 WrapBashCmd（Rust FFI 优先 → Go 降级）为执行后端。
//
// 调用链：ContainerSandbox（internal/sandbox）
//           → CmdRunner 接口
//           → WrapBashCmdRunner（此文件）
//           → RustSandboxExecV2
//
// 启动时由 cmd/polaris/boot_tools.go 注入到 NewContainerSandbox。

package sandbox

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	isandbox "github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// WrapBashCmdRunner 实现 isandbox.CmdRunner，无状态，可安全并发使用。
type WrapBashCmdRunner struct{}

// NewWrapBashCmdRunner 构造默认实现。
func NewWrapBashCmdRunner() *WrapBashCmdRunner { return &WrapBashCmdRunner{} }

// RunCmd 执行命令并等待完成，返回合并输出（stdout+stderr）、退出码和沙箱方法。
func (r *WrapBashCmdRunner) RunCmd(ctx context.Context, cfg isandbox.CmdRunnerCfg) ([]byte, int, string, error) {
	sandboxCtx := buildSandboxContext(cfg)
	resp, err := RustSandboxExecV2(sandboxCtx, cfg.TimeoutMs)
	if err != nil {
		// V2 无 Go 侧降级路径，network_block 时隔离工具不可用即拒绝，不吞错误、不裸跑。
		return nil, -1, "", apperr.Wrap(apperr.CodeInternal, "cmd_runner: RustSandboxExecV2 failed", err)
	}
	return []byte(resp.Output), resp.ExitCode, resp.SandboxMethod, nil
}

func buildSandboxContext(cfg isandbox.CmdRunnerCfg) protocol.SandboxContext {
	netPolicy := protocol.NetPolicyAllow
	if cfg.NetworkBlock {
		netPolicy = protocol.NetPolicyDeny
	}
	defaultDeny, defaultDenyWrite := policy.SandboxDenyPaths()
	return protocol.SandboxContext{
		CallerType:     cfg.CallerType,
		Command:        cfg.Command,
		Workdir:        cfg.WorkDir,
		AllowedPaths:   cfg.AllowedPaths,
		DenyPaths:      mergePaths(defaultDeny, cfg.DenyPaths),
		DenyWritePaths: mergePaths(defaultDenyWrite, cfg.DenyWritePaths),
		EnvExtra:       cfg.Env, // 已知差异：原来是完整替换环境，现在是叠加在 preset 之上
		NetworkPolicy:  netPolicy,
		TimeoutMs:      cfg.TimeoutMs,
	}
}

func mergePaths(defaults, extra []string) []string {
	seen := make(map[string]struct{}, len(defaults)+len(extra))
	merged := make([]string, 0, len(defaults)+len(extra))
	for _, path := range append(append([]string(nil), defaults...), extra...) {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		merged = append(merged, path)
	}
	return merged
}
