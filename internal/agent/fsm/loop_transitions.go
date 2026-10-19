package fsm

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// registerLoopTransitions 注册 Agentic 循环所需的状态机转移（ADR-0115 / WP2）。
// 包含 S_LOOP 与 S_LOOP_EXEC 的双向推进及完成退出。
func (sm *StateMachine) registerLoopTransitions() {
	// S_LOOP_RECALL → S_LOOP: 记忆召回完成，首条 user 消息已种入，进入第 1 轮 LLM 推理
	sm.add(Transition{
		From:    types.AgentStateLoopRecall,
		Trigger: types.TriggerLoopRecallDone,
		To:      types.AgentStateLoop,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{sm.loopEffect(sCtx)}, nil
		},
	})

	// S_LOOP → S_LOOP_EXEC: 模型返回了工具调用，进入执行阶段
	sm.add(Transition{
		From:    types.AgentStateLoop,
		Trigger: types.TriggerLoopStepDone,
		To:      types.AgentStateLoopExec,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{
				protocol.DeterministicEffect{
					Fn: sm.executeLoopStep,
				},
			}, nil
		},
	})

	// S_LOOP_EXEC → S_LOOP: 工具执行完成并已追加 tool 消息，进入下一轮 LLM 推理
	sm.add(Transition{
		From:    types.AgentStateLoopExec,
		Trigger: types.TriggerLoopExecDone,
		To:      types.AgentStateLoop,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			sCtx.Mu.Lock()
			sCtx.LoopRound++
			sCtx.Mu.Unlock()
			return []protocol.Effect{sm.loopEffect(sCtx)}, nil
		},
	})

	// S_LOOP_EXEC → S_LOOP_AWAIT: 派生子智能体或委派进入等待（R2-03）
	sm.add(Transition{
		From:    types.AgentStateLoopExec,
		Trigger: types.TriggerAwaitAgent,
		To:      types.AgentStateLoopAwait,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{protocol.DeterministicEffect{}}, nil
		},
	})

	// S_LOOP_AWAIT → S_LOOP_EXEC: 子智能体执行完成唤醒恢复（R2-03）
	sm.add(Transition{
		From:    types.AgentStateLoopAwait,
		Trigger: types.TriggerAgentHandoffDone,
		To:      types.AgentStateLoopExec,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{protocol.DeterministicEffect{Fn: sm.executeLoopStep}}, nil
		},
	})

	// S_LOOP → S_COMPLETE: 循环正常结束（auto 文本回复或模型显式调用 finish）
	sm.add(Transition{
		From:    types.AgentStateLoop,
		Trigger: types.TriggerLoopDone,
		To:      types.AgentStateComplete,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			sCtx.Mu.Lock()
			defer sCtx.Mu.Unlock()
			userQ := sCtx.RawIntentTS.UnsafeContent()
			finalReply := sCtx.PreparedReply
			if finalReply == "" {
				finalReply = sCtx.LoopSummary
			}
			if finalReply == "" {
				for i := len(sCtx.LoopMessages) - 1; i >= 0; i-- {
					if sCtx.LoopMessages[i].Role == "assistant" && sCtx.LoopMessages[i].Content != "" {
						finalReply = sCtx.LoopMessages[i].Content
						break
					}
				}
			}
			sCtx.PreparedReply = finalReply
			if userQ != "" && finalReply != "" {
				sCtx.ConversationHistory = append(sCtx.ConversationHistory,
					types.Message{Role: "user", Content: userQ},
					types.Message{Role: "assistant", Content: finalReply},
				)
			}
			sCtx.LoopCompletion = "auto"
			sCtx.PendingLoopCompletion = ""
			return nil, nil
		},
	})

	// S_LOOP → S_LOOP: 插话注入触发重新推理
	sm.add(Transition{
		From:    types.AgentStateLoop,
		Trigger: types.TriggerLoopSteer,
		To:      types.AgentStateLoop,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			sCtx.Mu.Lock()
			sCtx.LoopRound++
			sCtx.Mu.Unlock()
			return []protocol.Effect{sm.loopEffect(sCtx)}, nil
		},
	})

	// S_LOOP → S_LOOP: 崩溃恢复唤醒续跑（P1 / ADR-0104 / HE-6）
	sm.add(Transition{
		From:    types.AgentStateLoop,
		Trigger: types.TriggerResume,
		To:      types.AgentStateLoop,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return []protocol.Effect{sm.loopEffect(sCtx)}, nil
		},
	})

	// S_LOOP → S_FAILED: 循环推理失败，由合法的 TriggerReplanExhausted 转移到失败终态（R2-13）
	sm.add(Transition{
		From:    types.AgentStateLoop,
		Trigger: types.TriggerReplanExhausted,
		To:      types.AgentStateFailed,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			sCtx.Mu.Lock()
			sCtx.LoopEnding = true
			sCtx.TurnDegraded = true
			sCtx.Mu.Unlock()
			return nil, nil
		},
	})
}

// executeLoopRecall 是 S_LOOP_RECALL 阶段的 DeterministicEffect 存根（在 Agent 拦截）。
func (sm *StateMachine) executeLoopRecall(ctx context.Context, sCtx protocol.StateContext) (types.State, error) {
	return types.State("S_LOOP_RECALL_DONE"), nil
}

// executeLoopStep 是 S_LOOP_EXEC 阶段的 DeterministicEffect 存根。
// 真实工具执行在 Agent.executeDeterministicEffect 中拦截，保证与五防线和校验链一致。
func (sm *StateMachine) executeLoopStep(ctx context.Context, sCtx protocol.StateContext) (types.State, error) {
	return types.State("S_LOOP_EXEC_DONE"), nil
}

// LoopEffect 构造单轮 Agentic 调用的 LLMFillEffect（WP2 / ADR-0115）。
func (sm *StateMachine) LoopEffect(sCtx *StateContext) protocol.Effect {
	return sm.loopEffect(sCtx)
}

// loopEffect 构造单轮 Agentic 调用的 LLMFillEffect。
func (sm *StateMachine) loopEffect(sCtx *StateContext) protocol.Effect {
	th := config.CurrentThresholds().M4Kernel
	modelPool := th.ModelPoolPlanInitial
	if modelPool == "" {
		modelPool = "default"
	}

	return protocol.LLMFillEffect{
		SchemaRef: "agentic_loop",
		PromptFn: func(pCtx protocol.StateContext) []types.Message {
			return sm.promptLoop(sCtx, pCtx)
		},
		OnSuccess: func(pCtx protocol.StateContext, fill []byte) (types.State, error) {
			return sm.onLoopSuccess(sCtx, fill)
		},
		OnFailure: sm.onLoopFailure,
		MaxRetry:  1,
		ModelPool: modelPool,
	}
}

// promptLoop 组装当前轮次的 Agentic LLM 提示词。
func (sm *StateMachine) promptLoop(sCtx *StateContext, pCtx protocol.StateContext) []types.Message {
	ctx, cancel := sm.bgCtx()
	defer cancel()

	sCtx.Mu.Lock()
	if sCtx.LoopRound == 0 {
		sCtx.LoopRound = 1
	}
	rawTS := sCtx.RawIntentTS
	hasMsgs := len(sCtx.LoopMessages) > 0
	sCtx.Mu.Unlock()

	if !hasMsgs && !rawTS.IsEmpty() {
		sCtx.AppendLoopMessages(types.Message{
			Role:       "user",
			Content:    taint.Spotlighting(rawTS),
			TaintLevel: rawTS.Level(),
			Round:      1,
		})
	}

	sCtx.Mu.Lock()
	roundMsgs := make([]types.Message, len(sCtx.LoopMessages))
	copy(roundMsgs, sCtx.LoopMessages)

	var usageText string
	if sCtx.LoopLastUsageTokens > 0 || sCtx.LoopLastWallSeconds > 0 {
		usageText = formatUsageLine(sCtx.LoopLastUsageTokens, sCtx.LoopLastUsageCost, sCtx.LoopLastWallSeconds)
	}
	sCtx.Mu.Unlock()

	// 第一轮前缀组装时如果已有记忆系统，执行一次首轮召回并暂存，后续轮次直接复用
	var mem protocol.MemoryFacade
	if pCtx.Mem != nil {
		mem = pCtx.Mem
	}

	msgs, err := BuildAgenticPrompt(ctx, mem, sCtx, roundMsgs, usageText)
	if err != nil {
		msgs = fallbackAgenticPrompt(ctx, mem, sCtx, roundMsgs)
	}

	if sCtx.EpochTracker != nil {
		sCtx.Mu.Lock()
		sCtx.ContextEpoch = sCtx.EpochTracker.check(msgs)
		sCtx.Mu.Unlock()
	}

	return msgs
}

// drainSteerQueueToMessages 检查并出队插话队列，作为 user 消息注入模型上下文。
func drainSteerQueueToMessages(sCtx *StateContext) bool {
	sCtx.Mu.Lock()
	if len(sCtx.LoopSteerQueue) == 0 {
		sCtx.Mu.Unlock()
		return false
	}
	steers := make([]string, len(sCtx.LoopSteerQueue))
	copy(steers, sCtx.LoopSteerQueue)
	sCtx.LoopSteerQueue = nil
	round := sCtx.LoopRound
	sCtx.Mu.Unlock()

	for _, st := range steers {
		sCtx.AppendLoopMessages(types.Message{
			Role:    "user",
			Content: st,
			Round:   round,
		})
	}
	return true
}

// onLoopSuccess 处理单轮 LLM 输出判定。
func (sm *StateMachine) onLoopSuccess(sCtx *StateContext, fill []byte) (types.State, error) {
	sCtx.Mu.Lock()
	calls := make([]types.InferToolCall, len(sCtx.LoopPendingToolCalls))
	copy(calls, sCtx.LoopPendingToolCalls)
	sCtx.Mu.Unlock()

	// 检查是否有 finish 调用
	for _, call := range calls {
		if call.Name == "finish" {
			// 判定完成前先检查插话队列（R2-12）
			if drainSteerQueueToMessages(sCtx) {
				sCtx.Mu.Lock()
				sCtx.LoopPendingToolCalls = nil
				sCtx.Mu.Unlock()
				return types.State("S_LOOP_STEER"), nil
			}
			sCtx.Mu.Lock()
			sCtx.LoopEnding = true
			var sumArgs struct {
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(call.Input, &sumArgs); err == nil && sumArgs.Summary != "" {
				sCtx.LoopSummary = sumArgs.Summary
			} else {
				sCtx.LoopSummary = string(call.Input)
			}
			sCtx.LoopPendingToolCalls = nil
			sCtx.Mu.Unlock()
			metrics.RecordLoopTermination(context.Background(), "finish")
			return types.State("S_LOOP_DONE"), nil
		}
	}

	// 检查是否有常规工具调用
	if len(calls) > 0 {
		sCtx.Mu.Lock()
		sCtx.LoopConsecutiveTextRounds = 0
		sCtx.Mu.Unlock()
		return types.State("S_LOOP_STEP_DONE"), nil
	}

	// 连续纯文本轮次计数（R3-07）
	sCtx.Mu.Lock()
	sCtx.LoopConsecutiveTextRounds++
	consecutiveText := sCtx.LoopConsecutiveTextRounds
	completion := sCtx.LoopCompletion
	round := sCtx.LoopRound
	sCtx.Mu.Unlock()

	// 无工具调用且 auto 完成模式下，认为文本输出即收尾回复
	// 判定完成之前先检查插话队列，非空时不结束，注入插话后继续下一轮（R2-12）
	if drainSteerQueueToMessages(sCtx) {
		return types.State("S_LOOP_STEER"), nil
	}

	if completion == "" {
		completion = config.CurrentThresholds().M4Kernel.LoopCompletion
	}
	if completion == "explicit" {
		// 防浪费兜底：explicit 模式下，模型连续 2 轮只给文本、不调用 finish，就把最后一段文本当作 summary 收尾，并计数 polaris.agent.loop_termination_total{reason="implicit_finish"} (R3-07)。
		if consecutiveText >= 2 {
			sCtx.Mu.Lock()
			sCtx.LoopEnding = true
			sCtx.LoopSummary = getLastAssistantText(sCtx, fill)
			sCtx.Mu.Unlock()
			metrics.RecordLoopTermination(context.Background(), "implicit_finish")
			return types.State("S_LOOP_DONE"), nil
		}

		// 显式模式下未调用 finish 时要求模型确认完成并继续循环
		sCtx.AppendLoopMessages(types.Message{
			Role:    "user",
			Content: "系统提示：当前任务模式要求显式完成，请调用 finish 工具提供总结并结束任务。",
			Round:   round,
		})
		return types.State("S_LOOP_STEP_DONE"), nil
	}

	sCtx.Mu.Lock()
	sCtx.LoopEnding = true
	sCtx.Mu.Unlock()
	metrics.RecordLoopTermination(context.Background(), "auto")
	return types.State("S_LOOP_DONE"), nil
}

// onLoopFailure 处理 LLM 推理失败。
func (sm *StateMachine) onLoopFailure(sCtx protocol.StateContext, err error) (types.State, error) {
	return types.State("S_LOOP_FAILED"), err
}
