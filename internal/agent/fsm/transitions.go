package fsm

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/agent/dag"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// parsePlanOnSuccess 将 LLM 返回的 DAG JSON 解析为 DAGModel 并写入 sCtx，消除 S_PLAN / S_REPLAN 重复逻辑。
func parsePlanOnSuccess(sCtx *StateContext, pCtx protocol.StateContext, content []byte) (types.State, error) {
	var protocolPlan types.DAGModel
	if err := json.Unmarshal(content, &protocolPlan); err != nil {
		// LLM 输出无效 JSON，但已有预设/缓存的 DAGModel 时保留并继续。
		// 生产语义: 优先重用上一轮缓存计划，避免无效 LLM 输出导致立即重规划。
		if sCtx.DAGModel != nil {
			return "S_PLAN_DONE", nil
		}
		return "S_PLAN_FAILED", apperr.Wrap(apperr.CodeInternal, "failed to unmarshal DAGModel", err)
	}

	dependsMap := make(map[string][]string)
	for _, e := range protocolPlan.Edges {
		dependsMap[e.To] = append(dependsMap[e.To], e.From)
	}

	execNodes := make([]dag.ExecNode, len(protocolPlan.Nodes))
	for i, n := range protocolPlan.Nodes {
		argsBytes, _ := json.Marshal(n.Params)
		execNodes[i] = dag.ExecNode{
			ID:         n.ID,
			ToolName:   n.Action,
			Args:       argsBytes,
			DependsOn:  dependsMap[n.ID],
			TaintLevel: pCtx.MaxTaintLevel,
		}
	}
	execEdges := make([]dag.ExecEdge, len(protocolPlan.Edges))
	for i, e := range protocolPlan.Edges {
		execEdges[i] = dag.ExecEdge{From: e.From, To: e.To}
	}

	sCtx.DAGModel = &DAGModel{
		Nodes: execNodes,
		Edges: execEdges,
	}
	return "S_PLAN_DONE", nil
}

// registerTransitions 注册全部 10 条转移（spec/state.yaml §m4_par_state_machine）。
func (sm *StateMachine) registerTransitions() {
	// S_IDLE → S_SUSPENDED: Suspend-on-Idle 挂起
	sm.add(Transition{
		From:    types.AgentStateIdle,
		Trigger: types.TriggerSuspend,
		To:      types.AgentStateSuspended,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil // 持久化由上层 runFSM 显式触发
		},
	})

	// S_SUSPENDED → S_IDLE: 外部唤醒信号
	sm.add(Transition{
		From:    types.AgentStateSuspended,
		Trigger: types.TriggerResume,
		To:      types.AgentStateIdle,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_IDLE → S_PERCEIVE: 收到意图脉冲
	sm.add(Transition{
		From:    types.AgentStateIdle,
		Trigger: types.TriggerIntentReceived,
		To:      types.AgentStatePerceive,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "perceive_task",
					PromptFn: func(pCtx protocol.StateContext) []types.Message {
						return sm.promptPerceive(sCtx, pCtx)
					},
					OnSuccess: sm.onPerceiveSuccess,
					OnFailure: sm.onPerceiveFailure,
					MaxRetry:  1,
					ModelPool: "standard",
				},
			}, nil
		},
	})

	// S_PERCEIVE → S_PLAN: 任务理解完成
	sm.add(Transition{
		From:    types.AgentStatePerceive,
		Trigger: types.TriggerPerceiveDone,
		To:      types.AgentStatePlan,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			var originTaint types.TaintLevel
			if sCtx.RawIntentTS.Source.OriginTaintLevel != 0 {
				originTaint = sCtx.RawIntentTS.Source.OriginTaintLevel
			} else {
				originTaint = types.TaintMedium
			}
			thinkMode := metrics.SelectThinkingMode(sm.replanCount, originTaint, metrics.GlobalSurpriseIndex().Current())
			return []protocol.Effect{
				protocol.LLMFillEffect{
					ThinkingMode: thinkMode,
					SchemaRef:    "plan_dag",
					PromptFn: func(pCtx protocol.StateContext) []types.Message {
						return sm.promptPlan(sCtx, pCtx)
					},
					OnSuccess: func(pCtx protocol.StateContext, content []byte) (types.State, error) {
						return parsePlanOnSuccess(sCtx, pCtx, content)
					},
					OnFailure: sm.onPlanFailure,
					MaxRetry:  1,
					ModelPool: "reasoning",
				},
			}, nil
		},
	})

	// S_PLAN → S_VALIDATE: DAG 生成完成
	// 注意: Effects 函数在注册时就被截取，此时 sm 尚无法引用 Agent。
	// 因此实际的四层校验通过 Agent.runValidateDAG 在 executeEffect 中注入。
	sm.add(Transition{
		From:    types.AgentStatePlan,
		Trigger: types.TriggerPlanDone,
		To:      types.AgentStateValidate,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: sm.validateDAG,
				},
			}, nil
		},
	})

	// S_VALIDATE → S_EXECUTE: 四层校验通过
	sm.add(Transition{
		From:    types.AgentStateValidate,
		Trigger: types.TriggerValidateOk,
		To:      types.AgentStateExecute,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: sm.executeDAG,
				},
			}, nil
		},
	})

	// S_VALIDATE → S_REPLAN: 四层校验失败
	sm.add(Transition{
		From:    types.AgentStateValidate,
		Trigger: types.TriggerValidateFail,
		To:      types.AgentStateReplan,
		Guard: func(ctx context.Context, sCtx *StateContext) bool {
			return sm.replanCount < sCtx.MaxReplan
		},
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: func(ctx context.Context, sCtx protocol.StateContext) (types.State, error) {
						return types.State("S_REPLAN_DONE"), nil
					},
				},
			}, nil
		},
	})

	// S_EXECUTE → S_REFLECT: DAG 执行完成
	sm.add(Transition{
		From:    types.AgentStateExecute,
		Trigger: types.TriggerExecuteDone,
		To:      types.AgentStateReflect,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "reflect_result",
					PromptFn: func(pCtx protocol.StateContext) []types.Message {
						return sm.promptReflect(sCtx, pCtx)
					},
					OnSuccess: sm.onReflectSuccess,
					OnFailure: sm.onReflectFailure,
					MaxRetry:  0,
					ModelPool: "standard",
				},
			}, nil
		},
	})

	// S_EXECUTE → S_ROLLBACK: DAG 执行失败
	sm.add(Transition{
		From:    types.AgentStateExecute,
		Trigger: types.TriggerExecuteFail,
		To:      types.AgentStateRollback,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: sm.rollbackSaga,
				},
			}, nil
		},
	})

	// S_REFLECT → S_COMPLETE: 反思完成 ⇒ 正向终态
	sm.add(Transition{
		From:    types.AgentStateReflect,
		Trigger: types.TriggerReflectDone,
		To:      types.AgentStateComplete,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_ROLLBACK → S_REPLAN: Saga 逆序补偿完成
	sm.add(Transition{
		From:    types.AgentStateRollback,
		Trigger: types.TriggerRollbackDone,
		To:      types.AgentStateReplan,
		Guard: func(ctx context.Context, sCtx *StateContext) bool {
			return sm.replanCount < sCtx.MaxReplan
		},
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: func(ctx context.Context, sCtx protocol.StateContext) (types.State, error) {
						return types.State("S_REPLAN_DONE"), nil
					},
				},
			}, nil
		},
	})

	// S_REPLAN → S_PLAN: 重新规划
	sm.add(Transition{
		From:    types.AgentStateReplan,
		Trigger: types.TriggerReplanDone,
		To:      types.AgentStatePlan,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			var originTaint types.TaintLevel
			if sCtx.RawIntentTS.Source.OriginTaintLevel != 0 {
				originTaint = sCtx.RawIntentTS.Source.OriginTaintLevel
			} else {
				originTaint = types.TaintMedium
			}
			thinkMode := metrics.SelectThinkingMode(sm.replanCount, originTaint, metrics.GlobalSurpriseIndex().Current())
			return []protocol.Effect{
				protocol.LLMFillEffect{
					ThinkingMode: thinkMode,
					SchemaRef:    "plan_dag",
					PromptFn: func(pCtx protocol.StateContext) []types.Message {
						return sm.promptPlan(sCtx, pCtx)
					},
					OnSuccess: func(pCtx protocol.StateContext, content []byte) (types.State, error) {
						return parsePlanOnSuccess(sCtx, pCtx, content)
					},
					OnFailure: sm.onPlanFailure,
					MaxRetry:  1,
					ModelPool: "reasoning",
				},
			}, nil
		},
	})

	// S_REPLAN → S_FAILED: ReplanGuard 耗尽 ⇒ 负向终态
	sm.add(Transition{
		From:    types.AgentStateReplan,
		Trigger: types.TriggerReplanExhausted,
		To:      types.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_PERCEIVE → S_FAILED: 早期失败直接熔断
	sm.add(Transition{
		From:    types.AgentStatePerceive,
		Trigger: types.TriggerReplanExhausted,
		To:      types.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_PLAN → S_FAILED: 无法生成规划
	sm.add(Transition{
		From:    types.AgentStatePlan,
		Trigger: types.TriggerReplanExhausted,
		To:      types.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_REFLECT → S_FAILED: 无法反思
	sm.add(Transition{
		From:    types.AgentStateReflect,
		Trigger: types.TriggerReplanExhausted,
		To:      types.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})
}
