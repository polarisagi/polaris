package fsm

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// registerRespondTransitions 注册 S_RESPOND 相关转移（ADR-0098 决策二）。
// S_REFLECT → S_RESPOND 仍在 registerTransitions 里（原 S_REFLECT → S_COMPLETE 改指向），
// 这里只放 S_RESPOND 新增的入边与出边；拆文件是因为 transitions.go 已超 R7 行数上限。
func (sm *StateMachine) registerRespondTransitions() {
	// 直答：Perceive 判定 NeedsTools=false（S_PERCEIVE_DIRECT）。
	sm.add(Transition{
		From:    types.AgentStatePerceive,
		Trigger: types.TriggerRespondReady,
		To:      types.AgentStateRespond,
		Effects: sm.respondEffects,
	})
	// 直答：Plan 解析成功但 DAG 为空（S_PLAN_EMPTY），含 FastPath 无缓存 DAG。
	sm.add(Transition{
		From:    types.AgentStatePlan,
		Trigger: types.TriggerRespondReady,
		To:      types.AgentStateRespond,
		Effects: sm.respondEffects,
	})
	// 空回复自环重试：此时尚未推出任何 token，重来不会造成重复输出。
	sm.add(Transition{
		From:    types.AgentStateRespond,
		Trigger: types.TriggerRespondReady,
		To:      types.AgentStateRespond,
		Effects: sm.respondEffects,
	})
	sm.add(Transition{
		From:    types.AgentStateRespond,
		Trigger: types.TriggerRespondDone,
		To:      types.AgentStateComplete,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})
	sm.add(Transition{
		From:    types.AgentStateRespond,
		Trigger: types.TriggerReplanExhausted,
		To:      types.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})
}

func (sm *StateMachine) respondEffects(_ context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
	return []protocol.Effect{sm.respondEffect(sCtx)}, nil
}

// respondEffect 是全 FSM 唯一 Audience=User 的 LLMFillEffect（par_inv_06）。
// 不挂 tools、不要求结构化输出：这一步只写给人看的回复，行动已在 S_EXECUTE 完成。
func (sm *StateMachine) respondEffect(sCtx *StateContext) protocol.LLMFillEffect {
	const maxRetry = 1
	return protocol.LLMFillEffect{
		PromptFn: func(pCtx protocol.StateContext) []types.Message {
			return sm.promptRespond(sCtx, pCtx)
		},
		OnSuccess: func(pCtx protocol.StateContext, fill []byte) (types.State, error) {
			state, err := onRespondSuccess(pCtx, fill)
			if err != nil && sCtx.RespondAttempts < maxRetry {
				sCtx.RespondAttempts++
				slog.Warn("respond: empty reply, retrying", "attempt", sCtx.RespondAttempts, "err", err)
				return "S_RESPOND_RETRY", nil
			}
			return state, err
		},
		// 推理错误不重试：Router 已全量 failover（P-7），且流可能已推出部分 token。
		OnFailure: onRespondFailure,
		MaxRetry:  maxRetry,
		ModelPool: string(types.ModelPoolGeneral),
		Audience:  protocol.AudienceUser,
	}
}
