package agent_stop

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MakeAgentStopFn 构造 agent_stop 工具执行函数。
func MakeAgentStopFn() sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args types.AgentStopArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "agent_stop: invalid args", err)
		}
		if args.ID == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "agent_stop: id is required")
		}

		val := ctx.Value(protocol.CtxSubagentManagerKey{})
		if val == nil {
			return nil, apperr.New(apperr.CodeInternal, "agent_stop: SubagentManager not available in context")
		}

		mgr, ok := val.(protocol.SubagentManager)
		if !ok {
			return nil, apperr.New(apperr.CodeInternal, "agent_stop: invalid SubagentManager in context")
		}

		msg, err := mgr.AgentStop(ctx, args.ID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "agent_stop", err)
		}

		out := map[string]any{
			"status":   "stopped",
			"agent_id": args.ID,
			"message":  msg,
		}
		return json.Marshal(out)
	}
}
