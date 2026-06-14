package action

import (
	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// AssignSandboxTier 根据工具的风险等级和来源自动分配沙箱等级。
// 返回 (分配的 tier, error)。error 非 nil 时调用方不得执行工具。
// M07 §4.2: Tier0 上需要 SandboxContainer 的工具返回 ErrTier0SandboxLimit。
func AssignSandboxTier(tool protocol.Tool, hwTier int, goos string) (protocol.SandboxTier, error) {
	minTier := protocol.SandboxInProcess
	switch tool.Source {
	case protocol.ToolBuiltin:
		minTier = protocol.SandboxInProcess
	case protocol.ToolLLMGenerated, protocol.ToolMCP, protocol.ToolA2A:
		minTier = protocol.SandboxWasm // 规则 1：L2，非 L3
	}

	tier := minTier
	if tool.Capability >= protocol.CapWriteNetwork && tier < protocol.SandboxWasm {
		tier = protocol.SandboxWasm // 规则 2：WriteNetwork+ → L2 底线
	}
	if tool.Capability >= protocol.CapPrivileged {
		tier = protocol.SandboxContainer // 规则 2：Privileged → L3
	}

	if hasSideEffect(tool.SideEffects, protocol.SideProcessSpawn) {
		tier = protocol.SandboxContainer // 规则 3
	}

	// 规则 4：Tier0 上 Container 需求 → 拒绝（不降级，防安全底线突破）
	if tier == protocol.SandboxContainer && hwTier == 0 {
		return protocol.SandboxInProcess, perrors.ErrTier0SandboxLimit
	}

	return tier, nil
}

func hasSideEffect(effects []protocol.SideEffect, target protocol.SideEffect) bool {
	for _, e := range effects {
		if e == target {
			return true
		}
	}
	return false
}
