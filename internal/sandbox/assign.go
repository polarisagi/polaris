package sandbox

import (
	"github.com/polarisagi/polaris/pkg/types"
)

// AssignSandboxTier 根据工具的风险等级和来源自动分配沙箱等级。
// 返回 (分配的 tier, error)。error 非 nil 时调用方不得执行工具。
// M07 §4.2: Tier0 上 SandboxContainer 降级为 SandboxNativeOS（Rust 原生沙箱，无容器依赖）。
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
		// Tier-0 无容器运行时：降级到 SandboxNativeOS（Rust bwrap/Seatbelt）。
		// 不返回 error — Rust 原生沙箱在 2GB VPS 即可运行，功能等价。
		return types.SandboxNativeOS, nil
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
