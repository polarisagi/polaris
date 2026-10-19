package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runExecuteLoop 是 S_LOOP_EXEC 阶段的核心执行器。
// 负责失控保护检查、四层计划校验、并发执行工具、批量遮蔽与插话消费，并推进 FSM 回到 S_LOOP。
func (a *Agent) runExecuteLoop(ctx context.Context) error {
	a.sCtx.Mu.Lock()
	calls := a.sCtx.LoopPendingToolCalls
	a.sCtx.LoopPendingToolCalls = nil
	currentRound := a.sCtx.LoopRound
	limit := a.sCtx.LoopRunawayLimit
	a.sCtx.Mu.Unlock()

	// 1. 失控保护限制解析：若未设置，经 ProfilePolicy 获取档位默认值（full=0, standard=200, hardened=50）
	if limit <= 0 {
		profStr := ""
		if cfg := config.Get(); cfg != nil {
			profStr = cfg.Security.TrustProfile
		}
		prof := policy.TrustProfile(profStr).Normalize()
		pol := policy.ProfilePolicy(prof)
		limit = pol.RunawayGuardDefaultRounds
		a.sCtx.Mu.Lock()
		a.sCtx.LoopRunawayLimit = limit
		a.sCtx.Mu.Unlock()
	}

	// 2. 失控保护与预算检查
	if limit > 0 && currentRound >= limit {
		a.sCtx.Mu.Lock()
		warned := a.sCtx.LoopWarningInjected
		a.sCtx.Mu.Unlock()

		if !warned {
			// 首触上限：注入易变收尾提示消息，再给一轮机会
			a.sCtx.Mu.Lock()
			a.sCtx.LoopWarningInjected = true
			a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, types.Message{
				Role:    "system",
				Content: "失控保护告警：已达到回合最大轮数上限，请在本轮立即总结当前进度并给出最终回复或收尾。",
			})
			a.sCtx.Mu.Unlock()
			slog.WarnContext(ctx, "agentic_loop: runaway guard warning injected", "round", currentRound, "limit", limit)
		} else {
			// 次轮仍未收尾：硬熔断并保存进度
			metrics.RecordLoopTermination(ctx, "guard")
			guardErr := apperr.New(apperr.CodeResourceExhausted, fmt.Sprintf("runaway guard triggered: round %d >= limit %d", currentRound, limit))
			a.abortTurn(ctx, guardErr)
			return guardErr
		}
	}

	// 3. 构建单层批 DAGPlan，经过 S_VALIDATE 四层校验链
	if len(calls) > 0 {
		plan := toolCallsToDAGPlan(calls, a.sCtx.GlobalTaintLevel)
		if err := a.checkPlanAgainstProfile(plan); err != nil {
			slog.WarnContext(ctx, "agentic_loop: plan rejected by profile", "err", err)
			a.appendToolErrorMessages(calls, "Forbidden: tool violates agent profile policy: "+err.Error())
			a.asyncIntent(types.TriggerLoopExecDone)
			return nil
		}

		vCtx := a.buildValidationContext(plan)
		if a.dagValidator == nil {
			slog.ErrorContext(ctx, "agentic_loop: security validator is missing (fail-closed)")
			a.appendToolErrorMessages(calls, "Internal: security validator is missing (fail-closed)")
			a.asyncIntent(types.TriggerLoopExecDone)
			return nil
		}
		if err := a.validateWithTaintReview(ctx, vCtx); err != nil {
			slog.WarnContext(ctx, "agentic_loop: plan rejected by taint review", "err", err)
			a.appendToolErrorMessages(calls, "Forbidden: action rejected by security policy: "+err.Error())
			a.asyncIntent(types.TriggerLoopExecDone)
			return nil
		}
		a.recordValidatedPlan(plan)

		// 4. 并发执行当前批次工具调用
		toolResults := a.executeToolCallsConcurrently(ctx, calls)

		// 5. 追加 tool 响应消息到上下文
		a.sCtx.Mu.Lock()
		a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, toolResults...)
		a.sCtx.Mu.Unlock()
	}

	// 6. 执行 T3 批量观察遮蔽
	a.applyLoopObservationMasking(ctx)

	// 7. 消费插话队列（Steer），在轮次边界注入为 user 消息
	steers := a.drainSteerQueue()
	if len(steers) > 0 {
		a.sCtx.Mu.Lock()
		for _, st := range steers {
			a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, types.Message{
				Role:    "user",
				Content: st,
			})
		}
		a.sCtx.Mu.Unlock()
	}

	// 8. 记录指标与持久化事件
	metrics.RecordLoopRound(ctx)
	if a.sm != nil && a.sCtx != nil {
		eventID := a.sm.NextEventID(a.sCtx.SessionID, "loop_exec")
		_ = eventID
	}

	// 9. 推进状态机回到 S_LOOP 进入下一轮
	a.asyncIntent(types.TriggerLoopExecDone)
	return nil
}

// toolCallsToDAGPlan 将一批 InferToolCall 构造成单层批 DAGPlan，供四层校验链使用。
func toolCallsToDAGPlan(calls []types.InferToolCall, taintLevel types.TaintLevel) *protocol.DAGPlan {
	nodes := make([]protocol.ExecNode, len(calls))
	for i, c := range calls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("n%d", i)
		}
		nodes[i] = protocol.ExecNode{
			ID:         id,
			ToolName:   c.Name,
			Args:       c.Input,
			TaintLevel: taintLevel,
		}
	}
	return &protocol.DAGPlan{
		Nodes: nodes,
		Edges: nil,
	}
}

// appendToolErrorMessages 为因校验未通过而被拦截的调用构造错误响应。
func (a *Agent) appendToolErrorMessages(calls []types.InferToolCall, errText string) {
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()
	round := a.sCtx.LoopRound
	for _, c := range calls {
		a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, types.Message{
			Role:    "user",
			Content: errText,
			Round:   round,
			Parts: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": c.ID,
					"content":     errText,
				},
			},
		})
	}
}

// buildValidationContext 组装用于四层校验链的上下文对象。
func (a *Agent) buildValidationContext(plan *protocol.DAGPlan) *protocol.DAGValidationContext {
	return &protocol.DAGValidationContext{
		Plan:             plan,
		ActiveTaintLevel: maxNodeTaintLevel(plan),
		PolicyGate:       a.Security.PolicyGate,
		ToolExecutor:     a.toolRegistry,
		AgentID:          a.sCtx.AgentID,
		SessionID:        a.sCtx.SessionID,
		SystemTier:       a.Config.SystemTier,
		ReviewChecker:    a.Security.TaintReviewChecker,
		TaintAuditor:     a.Security.TaintAuditor,
	}
}
