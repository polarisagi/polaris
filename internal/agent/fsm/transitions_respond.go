package fsm

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// registerRespondTransitions 注册 S_RESPOND 相关转移（ADR-0098 决策二）。
// S_REFLECT → S_RESPOND 仍在 registerTransitions 里（原 S_REFLECT → S_COMPLETE 改指向），
// 这里只放 S_RESPOND 新增的入边与出边；拆文件是因为 transitions.go 已超 R7 行数上限。
func (sm *StateMachine) registerRespondTransitions() {
	// 直答：Perceive 判定 NeedsTools=false（S_PERCEIVE_DIRECT）。Perceive 已同次产出回复
	// 时不再调 LLM（ADR-0101 决策四 4b′）；只有这条入边消费 PreparedReply。
	sm.add(Transition{
		From:    types.AgentStatePerceive,
		Trigger: types.TriggerRespondReady,
		To:      types.AgentStateRespond,
		Effects: sm.directRespondEffects,
	})
	// 直答：Plan 解析成功但 DAG 为空（S_PLAN_EMPTY），含 FastPath 无缓存 DAG。
	sm.add(Transition{
		From:    types.AgentStatePlan,
		Trigger: types.TriggerRespondReady,
		To:      types.AgentStateRespond,
		Effects: sm.respondEffects,
	})
	// 空输出自环重试（ADR-0098 决策七）：S_RESPOND 此时尚未推出任何 token，
	// 重来不会造成重复输出；S_PLAN 重试不改变任何已执行状态。
	sm.add(Transition{
		From:    types.AgentStateRespond,
		Trigger: types.TriggerFillRetry,
		To:      types.AgentStateRespond,
		Effects: sm.respondEffects,
	})
	sm.add(Transition{
		From:    types.AgentStatePlan,
		Trigger: types.TriggerFillRetry,
		To:      types.AgentStatePlan,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{sm.planEffect(sCtx)}, nil
		},
	})
	// 观察—再规划（ADR-0098 决策八）：与校验失败 / 回滚共用 ReplanGuard，
	// 重规划计数与 ReplanDone 由 handleReplanTransition 统一处理。
	sm.add(Transition{
		From:    types.AgentStateReflect,
		Trigger: types.TriggerReflectContinue,
		To:      types.AgentStateReplan,
		Guard: func(ctx context.Context, sCtx *StateContext) bool {
			return sm.replanCount < sCtx.MaxReplan
		},
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
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

// directRespondEffects Perceive→Respond 入边。PreparedReply 非空时返回确定性 Effect：
// 回复正文由 Agent 在执行该 Effect 时以 AudienceUser 发布（agent 层 publishPreparedReply），
// S_RESPOND 仍是唯一向用户发布正文的状态（par_inv_06 不变），只是这一次不需要 LLM。
func (sm *StateMachine) directRespondEffects(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
	sCtx.Mu.RLock()
	prepared := sCtx.PreparedReply
	sCtx.Mu.RUnlock()
	if prepared == "" {
		return sm.respondEffects(ctx, sCtx)
	}
	return []protocol.Effect{protocol.DeterministicEffect{
		Fn: func(context.Context, protocol.StateContext) (types.State, error) {
			return "S_RESPOND_DONE", nil
		},
	}}, nil
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

// planEffect S_PLAN 的 LLM 填空（Perceive→Plan、Replan→Plan、空输出自环三处共用）。
//
// 空输出（既无正文也无工具调用，fill 为空）按 MaxRetry 自环重试：DeepSeek 思考模式
// 挂载工具时偶发只输出思考链就以 stop 结束（ADR-0098 决策七）。有内容但解析失败
// 不在此重试——那是契约违反，交给既有的缓存复用 / S_PLAN_FAILED 语义。
func (sm *StateMachine) planEffect(sCtx *StateContext) protocol.LLMFillEffect {
	const maxRetry = 1
	// ADR-0101 决策二：便宜池先行、失败再升级。复杂度缺失（Perceive 未解析/寒暄旁路）
	// 按 0 处理——走便宜池；真不够用会以校验失败/目标未达成进入重规划，届时升级。
	var complexity float64
	sCtx.Mu.RLock()
	if sCtx.TaskModel != nil {
		complexity = sCtx.TaskModel.Complexity
	}
	sCtx.Mu.RUnlock()
	surprise := metrics.GlobalSurpriseIndex().Current()
	thinking := metrics.SelectThinkingMode(sm.replanCount, complexity, surprise)
	pool := metrics.SelectPlanModelPool(sm.replanCount, complexity, surprise)
	if sCtx.PlanAttempts > 0 {
		// 空输出重试关闭思考：空输出的触发条件正是"思考模式 + 挂工具"（决策七实证），
		// 原样重试只是再掷一次同一枚骰子。
		thinking = types.ThinkingDisabled
	}
	return protocol.LLMFillEffect{
		ThinkingMode: thinking,
		SchemaRef:    "plan_dag",
		PromptFn: func(pCtx protocol.StateContext) []types.Message {
			return sm.promptPlan(sCtx, pCtx)
		},
		OnSuccess: func(pCtx protocol.StateContext, content []byte) (types.State, error) {
			if len(bytes.TrimSpace(content)) == 0 && sCtx.PlanAttempts < maxRetry {
				sCtx.PlanAttempts++
				slog.Warn("plan: empty output (no content, no tool calls), retrying", "attempt", sCtx.PlanAttempts)
				return "S_PLAN_RETRY", nil
			}
			return parsePlanOnSuccess(sCtx, pCtx, content)
		},
		OnFailure: sm.onPlanFailure,
		MaxRetry:  maxRetry,
		ModelPool: string(pool),
	}
}
