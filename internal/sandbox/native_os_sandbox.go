// Package sandbox — native_os_sandbox.go
//
// NativeOSSandbox 通过 Rust FFI（bwrap/Seatbelt/WSL2）执行 OS 级沙箱。
// 无需容器运行时，Tier-0（2GB VPS）可用。
//
// 设计依据:
//   - HE-Rule 2（可验证执行，物理断裂 > 概率过滤）
//   - ADR-0011（purego FFI 零 CGO）
//   - ADR-0025（Tier-0 P0 修复：SandboxNativeOS 替代 ErrTier0SandboxLimit）
//
// 循环 import 规避：
//   - internal/tool/sandbox（FFI 绑定层）反向引用 internal/sandbox（接口层）。
//   - 故此文件不能直接 import internal/tool/sandbox。
//   - 复用已有 CmdRunner 接口（cmd_runner.go 定义，WrapBashCmdRunner 注入）。
//   - bwrap V1 的 AllowedPaths 机制：scriptDir 进入白名单后，
//     --bind-try /tmp /tmp 会覆盖 --tmpfs /tmp，host /tmp 内容可见。
//
// 适用场景:
//   - SandboxNativeOS tier — CodeAct Python/Bash 脚本执行（LLM 生成代码）
//   - Tier-0 硬件（2GB VPS）上 SandboxContainer 的 fallback

package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// NativeOSSandbox 通过 Rust FFI 直接调用 OS 隔离原语执行脚本。
// 注入 CmdRunner（= WrapBashCmdRunner）以规避与 internal/tool/sandbox 的循环 import。
type NativeOSSandbox struct {
	runner CmdRunner // WrapBashCmdRunner（bwrap/Seatbelt）
}

// NewNativeOSSandbox 构造 NativeOSSandbox。
// runner == nil 时自动使用 NopCmdRunner（测试安全降级）。
func NewNativeOSSandbox(runner CmdRunner) *NativeOSSandbox {
	if runner == nil {
		runner = NopCmdRunner{}
	}
	return &NativeOSSandbox{runner: runner}
}

// Run 实现 SandboxProvider 接口。
// 支持 ScriptPath（CodeAct/Skill 脚本文件）或 Command（Hook 引擎任意 shell 命令）两种路径；
// 二者均为空时返回不支持错误，避免静默降级。
func (s *NativeOSSandbox) Run(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	if spec.ScriptPath != "" {
		return s.runScript(ctx, spec)
	}
	if spec.Command != "" {
		return s.runRawCommand(ctx, spec)
	}
	// NativeOSSandbox 专为脚本/命令执行设计；纯 ToolName 分发的工具调用不应路由至此。
	return &types.ToolResult{
		Success: false,
		Error:   "NativeOSSandbox: ScriptPath or Command required — non-script tool calls must use InProcessSandbox",
	}, nil
}

// runRawCommand 通过 CmdRunner 执行任意 shell 命令字符串（bash -c 语义），Tier-0 版本。
// 供 Hook 引擎在 2GB VPS（无容器运行时）上执行 hooks.yaml 配置的命令。
func (s *NativeOSSandbox) runRawCommand(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	quotaMs := uint64(spec.CPUQuotaMs)
	if quotaMs == 0 {
		quotaMs = 30000
	}
	start := time.Now()
	out, exitCode, _, runErr := s.runner.RunCmd(ctx, CmdRunnerCfg{
		CallerType:   "hook",
		Command:      spec.Command,
		AllowedPaths: spec.AllowedPaths,
		Env:          append(containerBaseEnv(), spec.ExtraEnv...),
		NetworkBlock: true,
		TimeoutMs:    quotaMs,
	})
	latency := time.Since(start).Milliseconds()
	if runErr != nil {
		return &types.ToolResult{Success: false, Error: runErr.Error(), LatencyMs: latency}, nil //nolint:nilerr
	}
	if exitCode != 0 {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("command exited with code %d", exitCode), Output: out, LatencyMs: latency}, nil
	}
	return &types.ToolResult{Success: true, Output: out, LatencyMs: latency}, nil
}

// runScript 通过 CmdRunner（→ Rust bwrap/Seatbelt）执行脚本文件。
//
// 路径可见性保证：scriptDir 加入 AllowedPaths，bwrap V1 的 --bind-try /tmp /tmp
// 在 --tmpfs /tmp 之后执行，host /tmp 内容可见（bind-mount 覆盖 tmpfs）。
func (s *NativeOSSandbox) runScript(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	interp, err := resolveInterpreter(spec.ScriptPath)
	if err != nil {
		// 解释器未找到作为工具级错误上报，不向调用方传播（与 ContainerSandbox 行为一致）
		return &types.ToolResult{Success: false, Error: err.Error()}, nil //nolint:nilerr
	}

	scriptDir := filepath.Dir(spec.ScriptPath)
	allowedPaths := make([]string, 0, len(spec.AllowedPaths)+1)
	allowedPaths = append(allowedPaths, spec.AllowedPaths...)
	// 脚本所在目录（通常 /tmp）加入白名单：
	// bwrap 会将 --bind-try /tmp /tmp 置于 --tmpfs /tmp 之后，
	// 使 host /tmp 内容对沙箱可见（CodeAct 临时脚本不会丢失）。
	if scriptDir != "" && scriptDir != "." && !nativeOSPathContains(allowedPaths, scriptDir) {
		allowedPaths = append(allowedPaths, scriptDir)
	}

	quotaMs := uint64(spec.CPUQuotaMs)
	if quotaMs == 0 {
		quotaMs = 30000
	}

	callerType := "skill"
	switch {
	case strings.HasPrefix(spec.ToolName, "codeact:"):
		callerType = "codeact"
	case strings.HasPrefix(spec.ToolName, "hook:"):
		callerType = "hook"
	}

	start := time.Now()
	out, exitCode, method, runErr := s.runner.RunCmd(ctx, CmdRunnerCfg{
		CallerType:   callerType,
		Command:      interp + " " + spec.ScriptPath,
		WorkDir:      scriptDir,
		AllowedPaths: allowedPaths,
		Env:          append(containerBaseEnv(), spec.ExtraEnv...), // 语言运行时变量，不含凭据（R1.15）+ 调用方追加变量
		NetworkBlock: true,                                         // CodeAct 生成代码 / Hook 脚本默认断网
		TimeoutMs:    quotaMs,
	})
	latency := time.Since(start).Milliseconds()

	if runErr != nil {
		// 沙箱启动/执行失败：error 编码进 ToolResult.Error，Go error 返回 nil（与 ContainerSandbox 行为一致）。
		return &types.ToolResult{Success: false, Error: runErr.Error(), LatencyMs: latency}, nil //nolint:nilerr
	}
	if exitCode != 0 {
		return &types.ToolResult{
			Success:    false,
			Error:      fmt.Sprintf("script exited with code %d (sandbox=%s)", exitCode, method),
			Output:     out,
			LatencyMs:  latency,
			TaintLevel: spec.TaintLevel,
		}, nil
	}
	return &types.ToolResult{
		Success:    true,
		Output:     out,
		LatencyMs:  latency,
		TaintLevel: spec.TaintLevel,
	}, nil
}

// nativeOSPathContains 检查 paths 切片是否已包含 target（避免重复绑定）。
func nativeOSPathContains(paths []string, target string) bool {
	for _, p := range paths {
		if strings.EqualFold(filepath.ToSlash(p), filepath.ToSlash(target)) {
			return true
		}
	}
	return false
}
