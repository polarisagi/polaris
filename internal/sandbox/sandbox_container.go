package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── Tier 3: ContainerSandbox ────────────────────────────────────────────────

// ContainerSandbox 通过 Rust 原生沙箱（bwrap/Seatbelt）执行特权工具。
// 统一使用 CmdRunner 接口，由 WrapBashCmdRunner 提供 Rust FFI + Go 降级实现。
// 适用于: types.CapPrivileged / TypeScript 脚本技能 / LLM 生成代码执行
//
// 架构决策: 统一 Rust 沙箱，废弃 Linux namespace / Firecracker 路径。
// 跨平台: macOS=Seatbelt, Linux=bwrap, Windows=WSL2（均通过 CmdRunner 抽象）。
type ContainerSandbox struct {
	binPath  string // 沙箱执行器二进制路径（如 /usr/local/bin/polaris-sandbox）
	platform string
	hwTier   probe.Tier
	runner   CmdRunner // Rust FFI + Go 降级命令执行器，启动时注入
}

// NewContainerSandbox 构造 ContainerSandbox。
// runner 为 nil 时自动使用 NopCmdRunner（测试环境安全降级）。
func NewContainerSandbox(binPath, platform string, hwTier probe.Tier, runner CmdRunner) *ContainerSandbox {
	if runner == nil {
		runner = NopCmdRunner{}
	}
	return &ContainerSandbox{binPath: binPath, platform: platform, hwTier: hwTier, runner: runner}
}

// Level 返回沙箱级别（实现 protocol.SandboxProvider）。
func (s *ContainerSandbox) Level() int { return 3 }

func (s *ContainerSandbox) Run(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	quotaMs := spec.CPUQuotaMs
	if quotaMs == 0 {
		quotaMs = 30000
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(quotaMs)*time.Millisecond)
	defer cancel()

	// CodeAct / 技能脚本直接执行路径：无需 Firecracker/VZ 基础设施，
	// 由 runNativeScript 通过 OS 命名空间隔离直接执行脚本文件。
	if spec.ScriptPath != "" {
		return s.runNativeScript(execCtx, spec)
	}

	// Hook 引擎路径：cfg.Command 是 hooks.yaml 里配置的任意 shell 命令（bash -c 语义），
	// 不是脚本文件路径，走独立分发（复用 RunHook 同款 CmdRunner 调用形状）。
	if spec.Command != "" {
		return s.runRawCommand(execCtx, spec)
	}

	// L3 非脚本路径：通过 Rust 沙箱执行 polaris-sandbox 二进制。
	// Firecracker/VZ/WSL2/native 后端已统一为 CmdRunner（bwrap/Seatbelt）。
	if s.binPath == "" {
		return &types.ToolResult{Success: false, Error: "ContainerSandbox: no sandbox binary path configured"}, nil
	}

	// 始终消毒环境变量，防止父进程凭据泄漏（R1.15）。
	env := sandboxMinEnv()
	if spec.DryRunMode {
		mockTable := make(map[string]MockResponse)
		proxy, proxyAddr, errProxy := NewMockProxy(mockTable)
		if errProxy != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "create mock proxy failed", errProxy)
		}
		defer proxy.Close()
		for k, v := range proxy.EnvVars() {
			env = append(env, k+"="+v)
		}
		_ = proxyAddr
	}

	command := s.binPath + " --tool " + spec.ToolName
	start := time.Now()
	out, exitCode, _, runErr := s.runner.RunCmd(execCtx, CmdRunnerCfg{
		// 这是 privileged 路径，语义上最贴近 builtin（启发式默认值，仅影响 env 丰富度）
		CallerType:   "builtin",
		Command:      command,
		AllowedPaths: spec.AllowedPaths,
		Env:          env,
		NetworkBlock: true,
		TimeoutMs:    uint64(quotaMs),
	})
	latency := time.Since(start).Milliseconds()
	if runErr != nil {
		if errors.Is(runErr, fs.ErrPermission) {
			metrics.GlobalSurpriseIndex().InjectFaultSignal(0.8)
		}
		return &types.ToolResult{Success: false, Error: runErr.Error(), LatencyMs: latency}, nil
	}
	if exitCode != 0 {
		return &types.ToolResult{
			Success:   false,
			Error:     fmt.Sprintf("sandbox binary exited with code %d", exitCode),
			Output:    out,
			LatencyMs: latency,
		}, nil
	}
	return &types.ToolResult{Success: true, Output: out, LatencyMs: latency}, nil
}

// RunHook 通过 Rust 沙箱执行插件 Hook 脚本（如 uninstall hook）。
// 替代原 Linux namespace 隔离，统一走 bwrap（Linux）/ Seatbelt（macOS）。
// workDir 为脚本工作目录；超时固定 30s。
func (s *ContainerSandbox) RunHook(ctx context.Context, scriptPath, workDir string) error {
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, exitCode, _, runErr := s.runner.RunCmd(execCtx, CmdRunnerCfg{
		CallerType:   "hook",
		Command:      scriptPath,
		WorkDir:      workDir,
		AllowedPaths: []string{workDir},
		Env:          sandboxMinEnv(), // 防止父进程凭据泄漏（R1.15）
		NetworkBlock: true,
		TimeoutMs:    30000,
	})
	if runErr != nil {
		if errors.Is(runErr, fs.ErrPermission) {
			metrics.GlobalSurpriseIndex().InjectFaultSignal(0.8)
		}
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("sandbox: RunHook %q", scriptPath), runErr)
	}
	if exitCode != 0 {
		return apperr.New(apperr.CodeInternal,
			fmt.Sprintf("sandbox: RunHook %q exited %d: %s", scriptPath, exitCode, string(out)))
	}
	return nil
}

// runRawCommand 通过 CmdRunner 执行任意 shell 命令字符串（bash -c 语义）。
// 供 ExecEnvelope.Execute 的 SandboxSpec.Command 分支使用，当前唯一调用方是 Hook 引擎
// （internal/action/hook/runner.go）——hooks.yaml 里的 command 字段是任意 shell 命令，
// 不是脚本文件路径，不适用 runNativeScript 的 ScriptPath 分发。
func (s *ContainerSandbox) runRawCommand(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	quotaMs := spec.CPUQuotaMs
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
		TimeoutMs:    uint64(quotaMs),
	})
	latency := time.Since(start).Milliseconds()
	if runErr != nil {
		if errors.Is(runErr, fs.ErrPermission) {
			metrics.GlobalSurpriseIndex().InjectFaultSignal(0.8)
		}
		return &types.ToolResult{Success: false, Error: runErr.Error(), LatencyMs: latency}, nil
	}
	if exitCode != 0 {
		return &types.ToolResult{
			Success:   false,
			Error:     fmt.Sprintf("command exited with code %d", exitCode),
			Output:    out,
			LatencyMs: latency,
		}, nil
	}
	return &types.ToolResult{Success: true, Output: out, LatencyMs: latency}, nil
}

func (s *ContainerSandbox) RunScript(ctx context.Context, skillName, scriptPath string, input []byte, trustTier types.TrustTier) ([]byte, error) {
	tool := types.Tool{Name: skillName, Source: types.ToolLLMGenerated, TrustTier: trustTier}
	tier, err := AssignSandboxTier(tool, tool.TrustTier, int(s.hwTier), s.platform)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeSandboxTier0Limit, "skill: tier rejected", err)
	}
	res, err := s.Run(ctx, SandboxSpec{
		ToolName: skillName, Input: input, SandboxTier: tier,
		ScriptPath: scriptPath, CPUQuotaMs: 30000,
	})
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return nil, apperr.New(apperr.CodeInternal, "skill: script failed: "+res.Error)
	}
	return res.Output, nil
}

// buildFirecrackerCmd / buildVZCmd / buildWSL2Cmd / buildNativeCmd 已删除。
// 统一由 ContainerSandbox.runner（CmdRunner）处理，后端为 Rust bwrap/Seatbelt。

// runNativeScript 通过 Rust 沙箱执行脚本文件（CodeAct + 技能脚本路径）。
// 解释器由脚本后缀推断：.py → python3, .sh/.bash → bash。
// 统一走 CmdRunner（bwrap/Seatbelt），替代原 Linux namespace 隔离。
// macOS 现在也有 Seatbelt 进程隔离（原来是零隔离）。
func (s *ContainerSandbox) runNativeScript(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	interp, err := resolveInterpreter(spec.ScriptPath)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, nil //nolint:nilerr // 解释器解析失败作为工具级错误上报，不向调用方传播
	}

	// 脚本目录加入白名单，确保沙箱可读取脚本本身（bwrap --bind-try 需要路径存在）。
	scriptDir := filepath.Dir(spec.ScriptPath)
	allowedPaths := make([]string, 0, len(spec.AllowedPaths)+1)
	allowedPaths = append(allowedPaths, spec.AllowedPaths...)
	if scriptDir != "" && scriptDir != "." {
		allowedPaths = append(allowedPaths, scriptDir)
	}

	// interp + 空格 + 脚本路径，由 bash -c 解释（WrapBashCmd 统一入口）。
	command := interp + " " + spec.ScriptPath

	// 区分 CodeAct / Skill / Hook（三者共用 ScriptPath 分发路径，靠 ToolName 前缀区分）
	callerType := "skill"
	switch {
	case strings.HasPrefix(spec.ToolName, "codeact:"):
		callerType = "codeact"
	case strings.HasPrefix(spec.ToolName, "hook:"):
		callerType = "hook"
	}

	start := time.Now()
	out, exitCode, _, runErr := s.runner.RunCmd(ctx, CmdRunnerCfg{
		CallerType:   callerType,
		Command:      command,
		WorkDir:      scriptDir,
		AllowedPaths: allowedPaths,
		Env:          append(containerBaseEnv(), spec.ExtraEnv...), // 生产环境基础变量，不含凭据（R1.15）+ 调用方追加变量
		NetworkBlock: true,
		TimeoutMs:    uint64(spec.CPUQuotaMs),
	})
	latency := time.Since(start).Milliseconds()
	if runErr != nil {
		if errors.Is(runErr, fs.ErrPermission) {
			metrics.GlobalSurpriseIndex().InjectFaultSignal(0.8)
		}
		return &types.ToolResult{Success: false, Error: runErr.Error(), LatencyMs: latency}, nil
	}
	if exitCode != 0 {
		return &types.ToolResult{
			Success:   false,
			Error:     fmt.Sprintf("script exited with code %d", exitCode),
			Output:    out,
			LatencyMs: latency,
		}, nil
	}
	return &types.ToolResult{Success: true, Output: out, LatencyMs: latency}, nil
}

// resolveInterpreter 从脚本后缀推断解释器绝对路径。
func resolveInterpreter(scriptPath string) (string, error) {
	switch strings.ToLower(filepath.Ext(scriptPath)) {
	case ".py":
		for _, name := range []string{"python3", "python"} {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
		}
		return "", apperr.New(apperr.CodeInternal, "sandbox: python3/python not found in PATH")
	case ".sh", ".bash":
		for _, name := range []string{"bash", "sh"} {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
		}
		return "", apperr.New(apperr.CodeInternal, "sandbox: bash/sh not found in PATH")
	default:
		return "", apperr.New(apperr.CodeInternal,
			fmt.Sprintf("sandbox: unsupported script extension %q", filepath.Ext(scriptPath)))
	}
}
