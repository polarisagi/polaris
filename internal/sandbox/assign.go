package sandbox

import (
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AssignSandboxTier 根据工具的风险等级和来源自动分配沙箱等级。
// 返回 (分配的 tier, error)。error 非 nil 时调用方不得执行工具。
// M07 §4.2: Tier0 上需要 SandboxContainer 的工具返回 ErrTier0SandboxLimit。
func AssignSandboxTier(tool types.Tool, trustTier types.TrustTier, hwTier int, goos string) (types.SandboxTier, error) {
	tier := trustTier.SandboxFloor() // 唯一信任源：参数

	var sourceTier types.SandboxTier
	switch tool.Source {
	case types.ToolBuiltin:
		sourceTier = types.SandboxInProcess
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
