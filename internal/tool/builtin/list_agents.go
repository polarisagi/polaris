package builtin

import (
	"context"

	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// list_agents / transfer_to_agent（ADR-0084 + ADR-0103 决策三）— consumer-side 接口定义
// （防 internal/tool/builtin (L1) 反向 import internal/extension (L2)，inv_NoCrossLayerImport）。
// 实现由 cmd/polaris 适配 *mcp.MCPManager 与 lifecycle.AgentDefinitionProvider 提供。
// ============================================================================

// A2AAgentDescriptor 是 mcp.MCPAgentDescriptor 的 consumer-side 镜像类型。
type A2AAgentDescriptor struct {
	Server      string
	Agent       string
	Description string
}

// A2AAgentLister 列出可通过 mcp: 前缀寻址的外部 A2A 委派目标。
type A2AAgentLister interface {
	ListA2AAgents(ctx context.Context) ([]A2AAgentDescriptor, error)
}

// LocalAgentDescriptor 本地子 Agent（插件 agents/、项目/用户 agents 目录）。
type LocalAgentDescriptor struct {
	Name        string
	Description string
	Source      string
}

// LocalAgentLister 列出本地可委派的子 Agent 定义。
type LocalAgentLister interface {
	ListLocalAgents(ctx context.Context) ([]LocalAgentDescriptor, error)
}

// GeneralPurposeAgent 无角色的通用子 Agent（Claude 内置 general-purpose 同名）。
const GeneralPurposeAgent = "general-purpose"

func listAgentsTool() types.Tool {
	return types.Tool{
		Name: catalog.ListAgentsToolName,
		Description: "List agents that transfer_to_agent can delegate to. Each entry's 'target' is the exact " +
			"string to pass as target_agent_role: local subagents defined by installed plugins or agent " +
			"definition files, external agents on connected MCP servers ('mcp:<server>/<agent>'), and " +
			"'general-purpose'. Pick the agent whose description matches the task.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapReadOnly,
		RiskLevel:   types.RiskLow,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// transferToAgentTool 委派工具的注册定义。执行由 Agent 内核特判（异步挂起 + Blackboard
// 投递，internal/agent/agent_handoff.go），此处注册使其进入模型可见目录并经 S_VALIDATE /
// PolicyGate 常规校验——此前从未注册，校验按"未知工具"拒绝，委派在生产中不可达。
func transferToAgentTool() types.Tool {
	return types.Tool{
		Name: catalog.DelegateToolName,
		Description: "Delegate a self-contained task to another agent and receive its final answer. " +
			"Use list_agents to discover valid targets. The delegated agent does not see this conversation: " +
			"context_summary must contain the complete task, relevant facts and the expected output.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
		Capability:  types.CapReadOnly, // 委派本身不产生副作用；子 Agent 的每次工具调用各自过校验
		RiskLevel:   types.RiskMedium,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target_agent_role": map[string]any{"type": "string", "description": "Target from list_agents."},
				"context_summary":   map[string]any{"type": "string", "description": "Complete, self-contained task description."},
			},
			"required": []string{"target_agent_role", "context_summary"},
		},
	}
}
