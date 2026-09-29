package agent

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// stateWantsTools 决定某 FSM 状态的 LLM 调用是否下发 tools 定义，以及是否同时禁止调用。
//
// 现状：仅 S_PLAN 下发 tools（原生 function-calling 通路，行为不变）。
// 实验开关 m4_kernel.cache.uniform_tools=true（ADR-0105 决策三）：Perceive/Reflect/Respond
// 也下发与 Plan 相同的 tools，并以 tool_choice=none 禁止调用——四阶段 tools 前缀字节一致，
// 让 tools（缓存前缀最靠前的部分）不再因阶段而失配。Validate 等其余状态从不携带 tools。
func stateWantsTools(state types.AgentState, uniform bool) (attach, forbidCall bool) {
	switch state {
	case types.AgentStatePlan:
		return true, false
	case types.AgentStatePerceive, types.AgentStateReflect, types.AgentStateRespond:
		return uniform, uniform
	}
	return false, false
}

// toolOptionsFor 把 schema 列表转换为 InferOption。schemas 为空返回 nil（不发空 tools）；
// forbidCall 时追加 tool_choice=none，适配器负责映射（不支持的适配器忽略该选项）。
func toolOptionsFor(schemas []types.ToolSchema, forbidCall bool) []types.InferOption {
	if len(schemas) == 0 {
		return nil
	}
	opts := []types.InferOption{types.WithTools(schemas)}
	if forbidCall {
		opts = append(opts, types.WithToolChoice("none"))
	}
	return opts
}

// toolInferOptions 返回本次 LLM 调用应附加的 tools 相关选项。
//
// TaskID 注入方式与 promptPlan 里 BuildToolListSection 保持一致：懒加载工具激活作用域需要
// 同一个 TaskID，否则上一轮 search_tools 激活的工具在本轮 Schemas() 重建时对不上
// （见 catalog/composite.go）。非 Plan 阶段读到的激活集合与 Plan 相同，故 tools 数组一致。
//
// 非 Plan 阶段即便模型仍返回 tool_calls：调用点只在 S_PLAN 才把 ToolCalls 转 DAG，
// 其余阶段沿用 resp.Content——tool_calls 被忽略，不会驱动执行。
func (a *Agent) toolInferOptions(ctx context.Context, state types.AgentState, uniform bool) []types.InferOption {
	attach, forbid := stateWantsTools(state, uniform)
	if !attach {
		return nil
	}
	cata := a.visibleCatalog()
	if cata == nil {
		return nil
	}
	toolCtx := context.WithValue(ctx, protocol.CtxTaskIDKey{}, a.sCtx.SessionID)
	return toolOptionsFor(cata.Schemas(toolCtx, types.TrustCommunity), forbid)
}
