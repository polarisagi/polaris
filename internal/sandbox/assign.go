package sandbox

import (
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AssignSandboxTier 根据工具的风险等级和来源自动分配沙箱等级。
// 返回 (分配的 tier, error)。error 非 nil 时调用方不得执行工具。
// M07 §4.2: Tier0 Container 请求全平台拒绝并返回 ErrTier0SandboxLimit（fail-closed）。
func AssignSandboxTier(tool types.Tool, trustTier types.TrustTier, hwTier int, goos string) (types.SandboxTier, error) {
	// 针对 Go 语言层面的执行载体（内置代码、MCP 桥接客户端），沙箱仅体现为 InProcess。
	// 实际进程隔离（如 MCP 的独立进程）由客户端层保证，不由 SandboxRouter 代理。
	if tool.Source == types.ToolBuiltin || tool.Source == types.ToolMCP {
		return types.SandboxInProcess, nil
	}

	tier := trustTier.SandboxFloor() // 唯一信任源：参数

	var sourceTier types.SandboxTier
	switch tool.Source {
	default:
		sourceTier = types.SandboxWasm
	}
	if sourceTier > tier {
		tier = sourceTier
	}
	if tool.Capability >= types.CapWriteNetwork && tier < types.SandboxWasm {
		tier = types.SandboxWasm
	}
	if tool.Capability >= types.CapPrivileged {
		tier = types.SandboxContainer
	}
	if hasSideEffect(tool.SideEffects, types.SideProcessSpawn) {
		tier = types.SandboxContainer
	}
	if tier == types.SandboxContainer && hwTier == 0 {
		// Tier-0 无容器运行时：根据 M07 §4.2，全平台拒绝，不降级 Wasm/NativeOS。
		return 0, apperr.ErrTier0SandboxLimit
	}
	return tier, nil
}

func hasSideEffect(effects []types.SideEffect, target types.SideEffect) bool {
	for _, e := range effects {
		if e == target {
			return true
		}
	}
	return false
}
