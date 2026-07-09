package sandbox

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// SandboxFacade 沙箱模块对外统一接口。
//
// 问题背景：
//
//	sandbox 包已有 SandboxProvider 接口（sandbox_impl.go），但上层代码（agent、action/codeact）
//	直接持有 *SandboxManager struct，导致与具体实现耦合。
//
// 解决方案：
//   - SandboxFacade 是 sandbox 包对外的统一入口接口
//   - 上层模块（agent、action/codeact、extension/skill）依赖此接口，不直接持有 SandboxManager
//   - 支持三级降级回退（Wasm → 容器 → InProcess），调用方无感
//
// @consumer: agent/agent.go, action/codeact.CodeAct, extension/skill.Executor
// @producer: sandbox.SandboxManager（由 cli.go/bootstrap 构造并注入 DependencyMap）
type SandboxFacade interface {
	// Run 在适当沙箱层级执行工具调用，自动降级回退（Tier3→Tier2→Tier1）。
	Run(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error)

	// RunAt 在指定层级执行（不降级），用于必须在特定沙箱等级的场景（如安全审计）。
	RunAt(ctx context.Context, tier types.SandboxTier, spec SandboxSpec) (*types.ToolResult, error)

	// RegisterInProcess 注册 Tier1 InProcess 工具函数（内置工具专用）。
	// 生产环境内置工具不走 Wasm/容器，直接在 Go goroutine 中执行。
	RegisterInProcess(toolName string, fn InProcessFn)

	// Level 返回当前系统可用的最高沙箱层级（由硬件探针决定）。
	Level() int
}
