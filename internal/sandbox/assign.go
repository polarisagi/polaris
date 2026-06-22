package sandbox

import (
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AssignSandboxTier 根据工具的风险等级和来源自动分配沙箱等级。
// 返回 (分配的 tier, error)。error 非 nil 时调用方不得执行工具。
// M07 §4.2: Tier0 上需要 SandboxContainer 的工具返回 ErrTier0SandboxLimit。
func AssignSandboxTier(tool types.Tool, hwTier int, goos string) (types.SandboxTier, error) {
	var minTier types.SandboxTier
	switch tool.Source {
	case types.ToolBuiltin:
		minTier = types.SandboxInProcess
	case types.ToolLLMGenerated, types.ToolMCP, types.ToolA2A:
		minTier = types.SandboxWasm // 规则 1：L2，非 L3
	default:
		minTier = types.SandboxWasm
	}

	tier := minTier
	if tool.Capability >= types.CapWriteNetwork && tier < types.SandboxWasm {
		tier = types.SandboxWasm // 规则 2：WriteNetwork+ → L2 底线
	}
	if tool.Capability >= types.CapPrivileged {
		tier = types.SandboxContainer // 规则 2：Privileged → L3
	}

	if hasSideEffect(tool.SideEffects, types.SideProcessSpawn) {
		tier = types.SandboxContainer // 规则 3
	}

	// 规则 4：Tier0 上 Container 需求 → 拒绝（不降级，防安全底线突破）
	if tier == types.SandboxContainer && hwTier == 0 {
		return types.SandboxInProcess, apperr.ErrTier0SandboxLimit
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
