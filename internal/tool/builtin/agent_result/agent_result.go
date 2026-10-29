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
		var args types.AgentResultArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "agent_result: invalid args", err)
		}
		if args.ID == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "agent_result: id is required")
		}

		val := ctx.Value(protocol.CtxSubagentManagerKey{})
		if val == nil {
			return nil, apperr.New(apperr.CodeInternal, "agent_result: SubagentManager not available in context")
		}

		mgr, ok := val.(protocol.SubagentManager)
		if !ok {
			return nil, apperr.New(apperr.CodeInternal, "agent_result: invalid SubagentManager in context")
		}

		result, err := mgr.AgentResult(ctx, args.ID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "agent_result", err)
		}

		out := map[string]any{
			"agent_id": args.ID,
			"result":   result,
		}
		return json.Marshal(out)
	}
}
