package builtin

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// listA2AAgentsResult 是单条委派目标的 LLM 侧响应形状。target 已拼装为
// "mcp:<server>/<agent>"，可直接原样用作 transfer_to_agent 的 target_agent_role。
type listA2AAgentsResult struct {
	Target      string `json:"target"`
	Description string `json:"description"`
}

// MakeListA2AAgentsFn 构造 list_a2a_agents 的执行函数（ADR-0084）。
// lister 为 nil 时返回空列表——没有已连接、具备 A2A 能力的 MCP Server 时的
// 自然状态，不视为错误（与 core_memory 系工具在依赖缺失时的降级方式一致）。
func MakeListA2AAgentsFn(lister A2AAgentLister) sandbox.InProcessFn {
	return func(ctx context.Context, _ []byte) ([]byte, error) {
		if lister == nil {
			out, _ := json.Marshal([]listA2AAgentsResult{}) //nolint:errchkjson // 固定空切片，Marshal 不会失败
			metrics.RecordMemoryToolCall(ctx, "list_a2a_agents", true)
			return out, nil
		}

		descriptors, err := lister.ListA2AAgents(ctx)
		if err != nil {
			metrics.RecordMemoryToolCall(ctx, "list_a2a_agents", false)
			return nil, apperr.Wrap(apperr.CodeInternal, "list_a2a_agents: ListA2AAgents failed", err)
		}

		out := make([]listA2AAgentsResult, 0, len(descriptors))
		for _, d := range descriptors {
			out = append(out, listA2AAgentsResult{
				Target:      "mcp:" + d.Server + "/" + d.Agent,
				Description: d.Description,
			})
		}
		result, merr := json.Marshal(out)
		if merr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "list_a2a_agents: marshal result", merr)
		}
		metrics.RecordMemoryToolCall(ctx, "list_a2a_agents", true)
		return result, nil
	}
}
