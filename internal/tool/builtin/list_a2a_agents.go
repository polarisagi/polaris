package builtin

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// list_a2a_agents（ADR-0084）— consumer-side 接口定义（防 internal/tool/builtin
// (L1) 反向 import internal/extension/mcp (L2)，inv_NoCrossLayerImport）。
// 实现由 cmd/polaris 适配 *mcp.MCPManager.ListA2AAgents 提供。
// ============================================================================

// A2AAgentDescriptor 是 mcp.MCPAgentDescriptor 的 consumer-side 镜像类型。
// 字段需与其保持同步；由 cmd/polaris 的适配器逐字段转换（不直接引用 mcp 包类型）。
type A2AAgentDescriptor struct {
	Server      string
	Agent       string
	Description string
}

// A2AAgentLister 列出可通过 mcp: 前缀寻址的外部 A2A 委派目标。
type A2AAgentLister interface {
	ListA2AAgents(ctx context.Context) ([]A2AAgentDescriptor, error)
}

func listA2AAgentsTool() types.Tool {
	return types.Tool{
		Name: "list_a2a_agents",
		Description: "List external agents reachable through connected MCP servers, for use with " +
			"transfer_to_agent's target_agent_role parameter. Each entry is already formatted as " +
			"'mcp:<server>/<agent>' — pass that exact string as target_agent_role to delegate a task to it. " +
			"Call this before using an mcp: target you have not already confirmed is currently available; " +
			"the list reflects only servers currently connected and exposing A2A delegation.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapReadOnly,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}
