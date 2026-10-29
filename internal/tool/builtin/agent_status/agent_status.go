package agent_status

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MakeAgentStatusFn 构造 agent_status 工具执行函数。
func MakeAgentStatusFn() sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args types.AgentStatusArgs
		if len(input) > 0 {
			if err := json.Unmarshal(input, &args); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "agent_status: invalid args", err)
			}
		}

		val := ctx.Value(protocol.CtxSubagentManagerKey{})
		if val == nil {
			return nil, apperr.New(apperr.CodeInternal, "agent_status: SubagentManager not available in context")
		}

		mgr, ok := val.(protocol.SubagentManager)
		if !ok {
			return nil, apperr.New(apperr.CodeInternal, "agent_status: invalid SubagentManager in context")
		}

		statuses, err := mgr.AgentStatus(ctx, args.IDs)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "agent_status", err)
		}

		out := map[string]any{
			"subagents": statuses,
			"count":     len(statuses),
		}
		return json.Marshal(out)
	}
}
