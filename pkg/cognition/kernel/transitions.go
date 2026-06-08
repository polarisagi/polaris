package kernel

import (
	"context"
	"encoding/json"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// parsePlanOnSuccess 将 LLM 返回的 DAG JSON 解析为 DAGModel 并写入 sCtx，消除 S_PLAN / S_REPLAN 重复逻辑。
func parsePlanOnSuccess(sCtx *StateContext, pCtx protocol.StateContext, content []byte) (protocol.State, error) {
	var protocolPlan protocol.DAGModel
	if err := json.Unmarshal(content, &protocolPlan); err != nil {
		// LLM 输出无效 JSON，但已有预设/缓存的 DAGModel 时保留并继续。
		// 生产语义: 优先重用上一轮缓存计划，避免无效 LLM 输出导致立即重规划。
		if sCtx.DAGModel != nil {
			return "S_PLAN_DONE", nil
		}
		return "S_PLAN_FAILED", perrors.Wrap(perrors.CodeInternal, "failed to unmarshal DAGModel", err)
	}

	dependsMap := make(map[string][]string)
	for _, e := range protocolPlan.Edges {
		dependsMap[e.To] = append(dependsMap[e.To], e.From)
	}

	execNodes := make([]ExecNode, len(protocolPlan.Nodes))
	for i, n := range protocolPlan.Nodes {
		argsBytes, _ := json.Marshal(n.Params)
		execNodes[i] = ExecNode{
			ID:         n.ID,
			ToolName:   n.Action,
			Args:       argsBytes,
			DependsOn:  dependsMap[n.ID],
			TaintLevel: pCtx.MaxTaintLevel,
		}
	}
	execEdges := make([]ExecEdge, len(protocolPlan.Edges))
	for i, e := range protocolPlan.Edges {
		execEdges[i] = ExecEdge{From: e.From, To: e.To}
	}

	sCtx.DAGModel = &DAGModel{
		Nodes: execNodes,
		Edges: execEdges,
	}
	return "S_PLAN_DONE", nil
}

// registerTransitions 注册全部 10 条转移（spec/state.yaml §m4_par_state_machine）。
func (sm *StateMachine) registerTransitions() {
	// S_IDLE → S_PERCEIVE: 收到意图脉冲
	sm.add(Transition{
		From:    protocol.AgentStateIdle,
		Trigger: protocol.TriggerIntentReceived,
		To:      protocol.AgentStatePerceive,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "perceive_task",
					PromptFn: func(pCtx protocol.StateContext) []protocol.Message {
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
		From:    protocol.AgentStatePerceive,
		Trigger: protocol.TriggerPerceiveDone,
		To:      protocol.AgentStatePlan,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "plan_dag",
					PromptFn: func(pCtx protocol.StateContext) []protocol.Message {
						return sm.promptPlan(sCtx, pCtx)
					},
					OnSuccess: func(pCtx protocol.StateContext, content []byte) (protocol.State, error) {
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
		From:    protocol.AgentStatePlan,
		Trigger: protocol.TriggerPlanDone,
		To:      protocol.AgentStateValidate,
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
		From:    protocol.AgentStateValidate,
		Trigger: protocol.TriggerValidateOk,
		To:      protocol.AgentStateExecute,
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
		From:    protocol.AgentStateValidate,
		Trigger: protocol.TriggerValidateFail,
		To:      protocol.AgentStateReplan,
		Guard: func(ctx context.Context, sCtx *StateContext) bool {
			return sm.replanCount < sCtx.MaxReplan
		},
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: func(ctx context.Context, sCtx protocol.StateContext) (protocol.State, error) {
						return protocol.State("S_REPLAN_DONE"), nil
					},
				},
			}, nil
		},
	})

	// S_EXECUTE → S_REFLECT: DAG 执行完成
	sm.add(Transition{
		From:    protocol.AgentStateExecute,
		Trigger: protocol.TriggerExecuteDone,
		To:      protocol.AgentStateReflect,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "reflect_result",
					PromptFn: func(pCtx protocol.StateContext) []protocol.Message {
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
		From:    protocol.AgentStateExecute,
		Trigger: protocol.TriggerExecuteFail,
		To:      protocol.AgentStateRollback,
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
		From:    protocol.AgentStateReflect,
		Trigger: protocol.TriggerReflectDone,
		To:      protocol.AgentStateComplete,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_ROLLBACK → S_REPLAN: Saga 逆序补偿完成
	sm.add(Transition{
		From:    protocol.AgentStateRollback,
		Trigger: protocol.TriggerRollbackDone,
		To:      protocol.AgentStateReplan,
		Guard: func(ctx context.Context, sCtx *StateContext) bool {
			return sm.replanCount < sCtx.MaxReplan
		},
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: func(ctx context.Context, sCtx protocol.StateContext) (protocol.State, error) {
						return protocol.State("S_REPLAN_DONE"), nil
					},
				},
			}, nil
		},
	})

	// S_REPLAN → S_PLAN: 重新规划
	sm.add(Transition{
		From:    protocol.AgentStateReplan,
		Trigger: protocol.TriggerReplanDone,
		To:      protocol.AgentStatePlan,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "plan_dag",
					PromptFn: func(pCtx protocol.StateContext) []protocol.Message {
						return sm.promptPlan(sCtx, pCtx)
					},
					OnSuccess: func(pCtx protocol.StateContext, content []byte) (protocol.State, error) {
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
		From:    protocol.AgentStateReplan,
		Trigger: protocol.TriggerReplanExhausted,
		To:      protocol.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_PERCEIVE → S_FAILED: 早期失败直接熔断
	sm.add(Transition{
		From:    protocol.AgentStatePerceive,
		Trigger: protocol.TriggerReplanExhausted,
		To:      protocol.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_PLAN → S_FAILED: 无法生成规划
	sm.add(Transition{
		From:    protocol.AgentStatePlan,
		Trigger: protocol.TriggerReplanExhausted,
		To:      protocol.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_REFLECT → S_FAILED: 无法反思
	sm.add(Transition{
		From:    protocol.AgentStateReflect,
		Trigger: protocol.TriggerReplanExhausted,
		To:      protocol.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})
}
