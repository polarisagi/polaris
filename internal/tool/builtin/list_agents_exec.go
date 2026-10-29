package builtin

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// listAgentsResult 单条委派目标。Target 可直接原样用作 transfer_to_agent 的 target_agent_role。
type listAgentsResult struct {
	Target      string `json:"target"`
	Description string `json:"description"`
	Kind        string `json:"kind"` // local / mcp / general
}

// MakeListAgentsFn 构造 list_agents 的执行函数。lister 为 nil 表示该类来源不可用（自然状态，非错误）；
// 任一来源出错整次失败——静默少列目标会让模型误以为目标不存在。
func MakeListAgentsFn(local LocalAgentLister, a2a A2AAgentLister) sandbox.InProcessFn {
	return func(ctx context.Context, _ []byte) ([]byte, error) {
		out := []listAgentsResult{{Target: GeneralPurposeAgent, Kind: "general",
			Description: "General agent with the same tools as you; use when no specialised agent fits."}}
		if local != nil {
			agents, err := local.ListLocalAgents(ctx)
			if err != nil {
				metrics.RecordMemoryToolCall(ctx, "list_agents", false)
				return nil, apperr.Wrap(apperr.CodeOf(err), "list_agents: local agents", err)
			}
			for _, d := range agents {
				out = append(out, listAgentsResult{Target: d.Name, Description: d.Description, Kind: "local"})
			}
		}
		if a2a != nil {
			descriptors, err := a2a.ListA2AAgents(ctx)
			if err != nil {
				metrics.RecordMemoryToolCall(ctx, "list_agents", false)
				return nil, apperr.Wrap(apperr.CodeInternal, "list_agents: ListA2AAgents failed", err)
			}
			for _, d := range descriptors {
				out = append(out, listAgentsResult{Target: "mcp:" + d.Server + "/" + d.Agent, Description: d.Description, Kind: "mcp"})
			}
		}
		result, err := json.Marshal(out)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "list_agents: marshal result", err)
		}
		metrics.RecordMemoryToolCall(ctx, "list_agents", true)
		return result, nil
	}
}

// transferToAgentFn 正常路径不会被调用（Agent 内核在分发前特判该工具名）；绕过内核直接经
// Dispatcher 调用时显式失败，而不是静默成功。
func transferToAgentFn(context.Context, []byte) ([]byte, error) {
	return nil, apperr.New(apperr.CodeInvalidInput, "transfer_to_agent is executed by the agent kernel and cannot be called directly")
}
