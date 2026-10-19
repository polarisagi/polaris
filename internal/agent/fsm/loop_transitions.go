package fsm

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// registerLoopTransitions 注册 Agentic 循环所需的状态机转移（ADR-0115 / WP2）。
// 包含 S_LOOP 与 S_LOOP_EXEC 的双向推进及完成退出。
func (sm *StateMachine) registerLoopTransitions() {
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

	// S_LOOP → S_COMPLETE: 循环正常结束（auto 文本回复或模型显式调用 finish）
	sm.add(Transition{
		From:    types.AgentStateLoop,
		Trigger: types.TriggerLoopDone,
		To:      types.AgentStateComplete,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
			return nil, nil
		},
	})

	// S_LOOP → S_LOOP: 插话注入触发重新推理
	sm.add(Transition{
		From:    types.AgentStateLoop,
		Trigger: types.TriggerLoopSteer,
		To:      types.AgentStateLoop,
		Effects: func(ctx context.Context, sCtx *StateContext) ([]protocol.Effect, error) {
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

// onLoopSuccess 处理单轮 LLM 输出判定。
func (sm *StateMachine) onLoopSuccess(sCtx *StateContext, fill []byte) (types.State, error) {
	sCtx.Mu.Lock()
	defer sCtx.Mu.Unlock()

	// 检查是否有 finish 调用
	for _, call := range sCtx.LoopPendingToolCalls {
		if call.Name == "finish" {
			sCtx.LoopSummary = string(call.Input)
			sCtx.LoopPendingToolCalls = nil
			return types.State("S_LOOP_DONE"), nil
		}
	}

	// 检查是否有常规工具调用
	if len(sCtx.LoopPendingToolCalls) > 0 {
		return types.State("S_LOOP_STEP_DONE"), nil
	}

	// 无工具调用且 auto 完成模式下，认为文本输出即收尾回复
	completion := sCtx.LoopCompletion
	if completion == "" {
		completion = config.CurrentThresholds().M4Kernel.LoopCompletion
	}
	if completion == "explicit" {
		// 显式模式下未调用 finish 时要求模型确认完成并继续循环
		sCtx.LoopMessages = append(sCtx.LoopMessages, types.Message{
			Role:    "user",
			Content: "系统提示：当前任务模式要求显式完成，请调用 finish 工具提供总结并结束任务。",
		})
		return types.State("S_LOOP_STEP_DONE"), nil
	}

	return types.State("S_LOOP_DONE"), nil
}

// onLoopFailure 处理 LLM 推理失败。
func (sm *StateMachine) onLoopFailure(sCtx protocol.StateContext, err error) (types.State, error) {
	return types.State("S_FAILED"), err
}

func formatUsageLine(tokens int, cost float64, wallSeconds float64) string {
	return "[usage] tokens=" + itoa(tokens) + " cost≈$" + formatCost(cost) + " wall=" + itoa(int(wallSeconds)) + "s"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	bp := len(b)
	neg := i < 0
	u := uint(i)
	if neg {
		u = uint(-i)
	}
	for u > 0 {
		bp--
		b[bp] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		bp--
		b[bp] = '-'
	}
	return string(b[bp:])
}

func formatCost(c float64) string {
	if c <= 0 {
		return "0.0000"
	}
	cents := int(c * 10000)
	intPart := cents / 10000
	fracPart := cents % 10000
	fracStr := itoa(fracPart)
	for len(fracStr) < 4 {
		fracStr = "0" + fracStr
	}
	return itoa(intPart) + "." + fracStr
}

func fallbackAgenticPrompt(ctx context.Context, mem protocol.MemoryFacade, sCtx *StateContext, roundMsgs []types.Message) []types.Message {
	b := prompt.NewPromptBuilder()
	if wErr := WriteSessionLayer(ctx, b, mem, sCtx); wErr != nil {
		slog.WarnContext(ctx, "agentic_loop: WriteSessionLayer fallback failed", "err", wErr)
	}
	WriteConversationHistory(b, sCtx)
	if len(roundMsgs) == 0 {
		sCtx.Mu.RLock()
		intent := sCtx.RawIntentTS
		sCtx.Mu.RUnlock()
		if !intent.IsEmpty() {
			b.WriteUserData(intent)
		}
		return FinishLayered(b, mem)
	}
	for _, m := range roundMsgs {
		b.WriteTurnMessage(m)
	}
	return FinishLayered(b, mem)
}
