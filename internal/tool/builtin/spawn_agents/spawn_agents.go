package spawn_agents

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MakeSpawnAgentsFn 构造 spawn_agents 工具执行函数。
func MakeSpawnAgentsFn() sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args types.SpawnAgentsArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "spawn_agents: invalid args", err)
		}

		val := ctx.Value(protocol.CtxSubagentManagerKey{})
		if val == nil {
			return nil, apperr.New(apperr.CodeInternal, "spawn_agents: SubagentManager not available in context")
		}

		mgr, ok := val.(protocol.SubagentManager)
		if !ok {
			return nil, apperr.New(apperr.CodeInternal, "spawn_agents: invalid SubagentManager in context")
		}

		ids, err := mgr.SpawnAgents(ctx, args)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "spawn_agents", err)
		}

		out := map[string]any{
			"status":    "dispatched",
			"agent_ids": ids,
			"count":     len(ids),
			"wait":      args.Wait,
		}
		return json.Marshal(out)
	}
}
