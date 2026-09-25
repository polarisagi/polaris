package fsm

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/agent/schemavalidate"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// parsePlanOnSuccess 将 LLM 返回的 DAG JSON 解析为 DAGModel 并写入 sCtx，消除 S_PLAN / S_REPLAN 重复逻辑。
//
// GR-4-005 复核修复：在 json.Unmarshal 之外新增 schemavalidate.Validate("plan_dag", ...)
// 结构校验，与"unmarshal 失败"合并进同一个降级分支——原实现只在 JSON 语法错误时
// 触发"复用上一轮缓存 DAGModel"的降级，但 json.Unmarshal 对语法合法、字段缺失/类型
// 兼容的输入（如节点对象缺 action 字段）不会报错，只会把该字段静默置零值，产出一个
// "看起来解析成功但语义不完整"的 DAGModel，直接进入 S_VALIDATE/S_EXECUTE 才在更下游
// 报出难以定位根因的错误（如 tool_name="" 导致工具查找失败）。二者现在走同一条判定：
// 只要内容不满足最低可用标准，就统一按"本轮 LLM 输出不可用"处理，不区分是语法错误
// 还是结构缺陷——调用方（S_PLAN/S_REPLAN 的既有降级测试）依赖的正是这一套统一语义，
// 不能只加校验不接入降级路径，也不能绕开降级路径直接判失败。
func parsePlanOnSuccess(sCtx *StateContext, pCtx protocol.StateContext, content []byte) (types.State, error) {
	// 模型惯用 ```json 围栏包裹输出，直接 Unmarshal 必败（首轮无缓存 DAG →
	// S_PLAN_FAILED → 重规划耗尽）。ExtractJSONBraces 未找到 JSON 时原样返回，
	// 错误照常在下面暴露。
	content = []byte(util.ExtractJSONBraces(string(content)))

	var protocolPlan types.DAGModel
	unmarshalErr := json.Unmarshal(content, &protocolPlan)
	reason := unmarshalErr
	if unmarshalErr == nil {
		reason = schemavalidate.Validate("plan_dag", content)
	}
	if reason != nil {
		// LLM 输出无效 JSON 或结构不完整（语法合法但缺必填字段/类型不符），但已有
		// 预设/缓存的 DAGModel 时保留并继续。生产语义: 优先重用上一轮缓存计划，
		// 避免无效 LLM 输出导致立即重规划。
		if sCtx.DAGModel != nil {
			slog.Warn("fsm: plan_dag content invalid, reusing cached DAGModel", "err", reason)
			return "S_PLAN_DONE", nil
		}
		return "S_PLAN_FAILED", apperr.Wrap(apperr.CodeInternal, "failed to unmarshal/validate DAGModel", reason)
	}

	dependsMap := make(map[string][]string)
	for _, e := range protocolPlan.Edges {
		dependsMap[e.To] = append(dependsMap[e.To], e.From)
	}

	execNodes := make([]protocol.ExecNode, len(protocolPlan.Nodes))
	for i, n := range protocolPlan.Nodes {
		argsBytes, _ := json.Marshal(n.Params)
		execNodes[i] = protocol.ExecNode{
			ID:         n.ID,
			ToolName:   n.Action,
			Args:       argsBytes,
			DependsOn:  dependsMap[n.ID],
			TaintLevel: pCtx.MaxTaintLevel,
		}
	}
	execEdges := make([]protocol.ExecEdge, len(protocolPlan.Edges))
	for i, e := range protocolPlan.Edges {
		execEdges[i] = protocol.ExecEdge{From: e.From, To: e.To}
	}

	sCtx.DAGModel = &DAGModel{
		Nodes: execNodes,
		Edges: execEdges,
	}
	// 解析成功但无节点 = 模型判定无需工具：直接合成回复，不空跑 Validate/Execute/Reflect。
	if len(execNodes) == 0 {
		metrics.RecordTurnRoute(context.Background(), routePlanEmpty)
		return "S_PLAN_EMPTY", nil
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
			if bypassEffect := sm.trySystem1Bypass(ctx, sCtx); bypassEffect != nil {
				return []protocol.Effect{bypassEffect}, nil
			}
			if phatic := tryPhaticBypass(sCtx); phatic != nil {
				return []protocol.Effect{phatic}, nil
			}
			// Unmatched case
			metrics.RecordSystem1Bypass(ctx, false)
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "perceive_task",
					PromptFn: func(pCtx protocol.StateContext) []types.Message {
						return sm.promptPerceive(sCtx, pCtx)
					},
					OnSuccess: func(_ protocol.StateContext, fill []byte) (types.State, error) {
						return sm.applyPerceiveResult(sCtx, fill)
					},
					OnFailure:      sm.onPerceiveFailure,
					MaxRetry:       1,
					ModelPool:      string(types.ModelPoolGeneral),
					ResponseFormat: &types.ResponseFormat{Type: "json_object"},
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
			surpriseIndex := metrics.GlobalSurpriseIndex().Current()
			if surpriseIndex < system1BypassSurpriseCeiling && sCtx.DAGModel != nil && len(sCtx.DAGModel.Nodes) > 0 {
				// SurpriseIndex 低于阈值，跳过 LLM 规划直接复用上次成功计划（GD-13-004）
				return []protocol.Effect{
					protocol.DeterministicEffect{
						Fn: func(ctx context.Context, pCtx protocol.StateContext) (types.State, error) {
							return types.State("S_PLAN_DONE"), nil
						},
					},
				}, nil
			}
			return []protocol.Effect{sm.planEffect(sCtx)}, nil
		},
	})

	// S_PERCEIVE → S_EXECUTE: System 1 Bypass 后四层校验通过
	sm.add(Transition{
		From:    types.AgentStatePerceive,
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

	// S_PERCEIVE → S_REPLAN: System 1 Bypass 后四层校验失败
	sm.add(Transition{
		From:    types.AgentStatePerceive,
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
			if skip := sm.trySkipReflect(sCtx); skip != nil {
				return []protocol.Effect{skip}, nil
			}
			return []protocol.Effect{
				protocol.LLMFillEffect{
					SchemaRef: "reflect_result",
					PromptFn: func(pCtx protocol.StateContext) []types.Message {
						return sm.promptReflect(sCtx, pCtx)
					},
					OnSuccess: func(pCtx protocol.StateContext, fill []byte) (types.State, error) {
						return sm.applyReflectResult(sCtx, pCtx, fill)
					},
					OnFailure:      sm.onReflectFailure,
					MaxRetry:       0,
					ModelPool:      string(types.ModelPoolGeneral),
					ResponseFormat: &types.ResponseFormat{Type: "json_object"},
				},
			}, nil
		},
	})

	// S_EXECUTE → S_ROLLBACK: 业务节点失败，触发 Saga 补偿
	sm.add(Transition{
		From:    types.AgentStateExecute,
		Trigger: types.TriggerExecuteFail,
		To:      types.AgentStateRollback,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{protocol.DeterministicEffect{Fn: sm.rollbackSaga}}, nil
		},
	})

	// S_EXECUTE → S_AWAIT_AGENT: 发起委派。真实的 checkpoint 持久化 +
	// watcher 启动在 executeDeterministicEffect（agent_execute_effect_
	// helpers.go）中按当前状态拦截处理，此转移本身无需额外 Effect
	// （沿用 protocol.DeterministicEffect 空载体，保持转移表结构一致）。
	sm.add(Transition{
		From:    types.AgentStateExecute,
		Trigger: types.TriggerAwaitAgent,
		To:      types.AgentStateAwaitAgent,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_AWAIT_AGENT → S_EXECUTE: 委派完成恢复。真实的结果回填由
	// runExecuteDAG 重新进入 executeTransferToAgent 的"恢复检查"分支完成。
	sm.add(Transition{
		From:    types.AgentStateAwaitAgent,
		Trigger: types.TriggerAgentHandoffDone,
		To:      types.AgentStateExecute,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_REFLECT → S_RESPOND: 反思完成 ⇒ 合成用户回复（ADR-0098，原直接进 S_COMPLETE，
	// 执行路径因此从不产出回复）。
	sm.add(Transition{
		From:    types.AgentStateReflect,
		Trigger: types.TriggerReflectDone,
		To:      types.AgentStateRespond,
		Effects: sm.respondEffects,
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

	// S_ROLLBACK → S_FAILED: Saga 逆序补偿部分失败，触发 ESCALATE
	sm.add(Transition{
		From:    types.AgentStateRollback,
		Trigger: types.TriggerRollbackPartial,
		To:      types.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_REPLAN → S_PLAN: 重新规划
	sm.add(Transition{
		From:    types.AgentStateReplan,
		Trigger: types.TriggerReplanDone,
		To:      types.AgentStatePlan,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{sm.planEffect(sCtx)}, nil
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

	sm.registerRespondTransitions()

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

// trySystem1Bypass 尝试短路 LLM 思考，直接命中已有技能并组装成验证态（GD-13-004）
// trySkipReflect 简单任务首轮执行全部成功时以确定性 Effect 代替 Reflect LLM
// （ADR-0101 决策四）。Reflect 的两个产出在此场景下价值最低：观察—再规划只对
// "看到结果才知道下一步"的多步任务有意义，而 Complexity<阈值 的任务按 Perceive
// 标尺就是"一两个显而易见的工具调用"；成功路径的 learnings 信息量也最低。
// 回复阶段照常拿到执行结果，未达成时由 Respond 如实说明。
// 任一条件不满足（重规划中、存在软失败、复杂度缺失或偏高）都走原 LLM 反思。
func (sm *StateMachine) trySkipReflect(sCtx *StateContext) protocol.Effect {
	gate := config.CurrentThresholds().M4Kernel.ReflectSkipComplexity
	sCtx.Mu.RLock()
	ok := gate > 0 && sm.replanCount == 0 && sCtx.ExecAllSucceeded &&
		sCtx.TaskModel != nil && sCtx.TaskModel.Complexity > 0 && sCtx.TaskModel.Complexity < gate
	sCtx.Mu.RUnlock()
	if !ok {
		return nil
	}
	return protocol.DeterministicEffect{
		Fn: func(ctx context.Context, _ protocol.StateContext) (types.State, error) {
			sCtx.Mu.Lock()
			sCtx.Reflection = nil // 不让上一轮的反思结论混入本轮回复
			sCtx.Mu.Unlock()
			metrics.RecordTurnRoute(ctx, routeReflectSkipped)
			return "S_REFLECT_DONE", nil
		},
	}
}

// tryPhaticBypass 寒暄/致谢/告别跳过 Perceive LLM 与记忆召回，直接进 S_RESPOND
// （ADR-0101 决策一）。等价于 Perceive 以 NeedsTools=false 返回，但省掉一次 LLM
// 往返与一轮 episodic/reflection/RAG 检索（含 embedding 调用）。
// 短确认（IntentAck）不走这里：它可能是对上一轮提议动作的授权，须经 Perceive 消解。
func tryPhaticBypass(sCtx *StateContext) protocol.Effect {
	sCtx.Mu.RLock()
	raw := sCtx.RawIntentTS
	sCtx.Mu.RUnlock()
	if raw.IsEmpty() || ClassifyIntentWeight(raw.UnsafeContent()) != IntentPhatic {
		return nil
	}
	return protocol.DeterministicEffect{
		Fn: func(ctx context.Context, _ protocol.StateContext) (types.State, error) {
			noTools := false
			sCtx.Mu.Lock()
			sCtx.TaskModel = &TaskModel{Goal: raw.UnsafeContent(), Complexity: 0.1, NeedsTools: &noTools}
			sCtx.Mu.Unlock()
			metrics.RecordTurnRoute(ctx, routePhatic)
			return "S_PERCEIVE_DIRECT", nil
		},
	}
}

func (sm *StateMachine) trySystem1Bypass(ctx context.Context, sCtx *StateContext) protocol.Effect {
	if !sCtx.HasPreMatch {
		return nil
	}
	// B-1：使用在 Dispatch 锁外提前预取的结果，防止锁内 IO 导致死锁（inv_FSM_B1）
	skillID, score, err := sCtx.PreMatchSkillID, sCtx.PreMatchScore, sCtx.PreMatchErr
	if err != nil || skillID == "" {
		return nil
	}
	if score < 0.92 {
		return nil
	}

	// 命中 System-1 Bypass
	metrics.RecordSystem1Bypass(ctx, true)
	sCtx.TaskModel = &TaskModel{
		Goal:       sCtx.RawIntentTS.UnsafeContent(),
		Complexity: 0.1,
	}
	sCtx.DAGModel = &DAGModel{
		Nodes: []protocol.ExecNode{
			{
				ID:       "bypass_node",
				ToolName: skillID,
			},
		},
	}

	// 直接返回 validateDAG，触发 FSM 去做四层校验，并在成功后返回 "S_VALIDATE_OK"
	return protocol.DeterministicEffect{
		Fn: sm.validateDAG,
	}
}
