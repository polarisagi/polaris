package agent_message

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MakeAgentMessageFn 构造 agent_message 工具执行函数。
func MakeAgentMessageFn() sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args types.AgentMessageArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "agent_message: invalid args", err)
		}
		if args.ID == "" || args.Text == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "agent_message: id and text are required")
		}

		val := ctx.Value(protocol.CtxSubagentManagerKey{})
		if val == nil {
			return nil, apperr.New(apperr.CodeInternal, "agent_message: SubagentManager not available in context")
		}

		mgr, ok := val.(protocol.SubagentManager)
		if !ok {
			return nil, apperr.New(apperr.CodeInternal, "agent_message: invalid SubagentManager in context")
		}

		msg, err := mgr.AgentMessage(ctx, args.ID, args.Text)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "agent_message", err)
		}

		out := map[string]any{
			"status":   "sent",
			"agent_id": args.ID,
			"message":  msg,
		}
		return json.Marshal(out)
	}
}
