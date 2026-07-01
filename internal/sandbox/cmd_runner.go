// Package sandbox — cmd_runner.go
//
// CmdRunner 是原生命令执行的抽象接口，供 ContainerSandbox 依赖注入。
//
// 设计原因：internal/sandbox 被 internal/tool/sandbox 反向引用（后者需要
// SandboxSpec/SandboxProvider 等类型），若 sandbox_impl.go 直接 import
// internal/tool/sandbox 会产生包循环。CmdRunner 接口在此定义，
// 实现由 internal/tool/sandbox.WrapBashCmdRunner 提供，启动时注入。
//
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2

package sandbox

import "context"

// CmdRunnerCfg 单次原生命令执行配置。
type CmdRunnerCfg struct {
	// Command 待执行的 shell 命令（由 bash -c 解释）。
	Command string
	// WorkDir 工作目录；空 = 继承调用方当前目录。
	WorkDir string
	// AllowedPaths 可读写路径白名单（bwrap --bind-try / Seatbelt file* subpath）。
	// 脚本文件所在目录应包含在内，否则沙箱无法读取脚本。
	AllowedPaths []string
	// Env 已清理的环境变量列表（KEY=VALUE 格式）。
	// 传 nil 时由实现方使用最小安全环境变量集合。
	Env []string
	// NetworkBlock true = 禁止所有出站网络（对齐 Claude Code 默认行为）。
	NetworkBlock bool
	// TimeoutMs 超时毫秒；0 = 实现方默认值（通常 30000ms）。
	TimeoutMs uint64
}

// CmdRunner 抽象原生命令执行器（Rust FFI + Go 降级路径）。
//
// 实现方：internal/tool/sandbox.WrapBashCmdRunner。
// 测试方：NopCmdRunner（本文件）。
type CmdRunner interface {
	// RunCmd 在原生沙箱中执行命令，返回合并输出（stdout+stderr）、
	// 退出码和沙箱方法标识（用于可观测性）。
	// exitCode != 0 时 err 为 nil，output 含错误输出；
	// err != nil 表示沙箱本身启动失败（非命令失败）。
	RunCmd(ctx context.Context, cfg CmdRunnerCfg) (output []byte, exitCode int, method string, err error)
}

// NopCmdRunner 空实现，始终返回成功空输出。
// 用于单元测试和 ContainerSandbox 未注入 runner 时的安全降级。
type NopCmdRunner struct{}

func (NopCmdRunner) RunCmd(_ context.Context, _ CmdRunnerCfg) ([]byte, int, string, error) {
	return []byte{}, 0, "nop", nil
}
