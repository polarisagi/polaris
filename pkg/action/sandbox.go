package action

import (
	"github.com/polarisagi/polaris/internal/protocol"
)

// AssignSandboxTier 根据工具的风险等级和来源自动分配沙箱等级。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §4.2
//
// 规则:
//  1. Source → 最小级别: Builtin→InProcess, LLMGenerated→Wasm, MCP/A2A→Wasm
//  2. Capability 提升: ReadOnly/WriteLocal/WriteNetwork→>=Wasm, Privileged→Container
//  3. SideProcessSpawn → Container
//  4. Tier0 非 Linux L2 降级
func AssignSandboxTier(tool protocol.Tool, hwTier int, goos string) protocol.SandboxTier {
	minTier := protocol.SandboxInProcess
	switch tool.Source {
	case protocol.ToolBuiltin:
		minTier = protocol.SandboxInProcess
	case protocol.ToolLLMGenerated:
		minTier = protocol.SandboxContainer
	case protocol.ToolMCP, protocol.ToolA2A:
		minTier = protocol.SandboxContainer
	}

	tier := minTier
	if tool.Capability >= protocol.CapWriteNetwork {
		tier = protocol.SandboxContainer
	}
	if tool.Capability >= protocol.CapPrivileged {
		tier = protocol.SandboxContainer
	}

	if hasSideEffect(tool.SideEffects, protocol.SideProcessSpawn) {
		tier = protocol.SandboxContainer
	}

	return tier
}

func hasSideEffect(effects []protocol.SideEffect, target protocol.SideEffect) bool {
	for _, e := range effects {
		if e == target {
			return true
		}
	}
	return false
}
