package agent_result

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MakeAgentResultFn 构造 agent_result 工具执行函数。
func MakeAgentResultFn() sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		result, err := MakeAgentResultRichFn()(ctx, sandbox.SandboxSpec{Input: input})
		if err != nil {
			return nil, err
		}
		return result.Output, nil
	}
}

// MakeAgentResultRichFn 将子任务保存的实际污点随查询结果一并返回。
func MakeAgentResultRichFn() sandbox.InProcessRichFn {
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		var args types.AgentResultArgs
		if err := json.Unmarshal(spec.Input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "agent_result: invalid args", err)
		}
		if args.ID == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "agent_result: id is required")
		}
		val := ctx.Value(protocol.CtxSubagentManagerKey{})
		mgr, ok := val.(protocol.SubagentManager)
		if !ok || mgr == nil {
			return nil, apperr.New(apperr.CodeInternal, "agent_result: SubagentManager not available in context")
		}
		result, resultTaint, err := mgr.AgentResultWithTaint(ctx, args.ID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "agent_result", err)
		}
		out, err := json.Marshal(map[string]any{"agent_id": args.ID, "result": result})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "agent_result: marshal result", err)
		}
		return &types.ToolResult{Success: true, Output: out, TaintLevel: types.EffectiveTaskResultTaint(resultTaint)}, nil
	}
}
