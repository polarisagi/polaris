package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runExecuteLoop 是 S_LOOP_EXEC 阶段的核心执行器。
// 负责失控保护检查、四层计划校验、并发执行工具、批量遮蔽与插话消费，并推进 FSM 回到 S_LOOP。
//
//nolint:gocyclo
func (a *Agent) runExecuteLoop(ctx context.Context) error {
	a.sCtx.Mu.Lock()
	isHandoffResume := len(a.sCtx.HandoffTaskIDs) > 0 && len(a.sCtx.LoopHandoffSpawnCallID) > 0
	a.sCtx.Mu.Unlock()

	if isHandoffResume {
		return a.resumeHandoffInLoop(ctx)
	}

	a.sCtx.Mu.Lock()
	calls := a.sCtx.LoopPendingToolCalls
	a.sCtx.LoopPendingToolCalls = nil
	currentRound := a.sCtx.LoopRound
	limit := a.sCtx.LoopRunawayLimit
	budgetUSD := a.sCtx.LoopTurnBudgetUSD
	budgetTokens := a.sCtx.LoopTurnBudgetTokens
	budgetWallSec := a.sCtx.LoopTurnWallSeconds
	currentTokens := a.sCtx.LoopLastUsageTokens
	currentCost := a.sCtx.LoopLastUsageCost
	currentWallSec := a.sCtx.LoopLastWallSeconds
	if currentWallSec == 0 && !a.sCtx.LoopTurnStartTime.IsZero() {
		currentWallSec = time.Since(a.sCtx.LoopTurnStartTime).Seconds()
		a.sCtx.LoopLastWallSeconds = currentWallSec
	}
	a.sCtx.Mu.Unlock()

	// 1. 失控保护限制解析：配置显式值 > 档位默认值（full=0, standard=200, hardened=50）
	th := config.CurrentThresholds().M4Kernel
	if limit <= 0 {
		if th.LoopRunawayGuardRounds > 0 {
			limit = th.LoopRunawayGuardRounds
		} else {
			profStr := ""
			if cfg := config.Get(); cfg != nil {
				profStr = cfg.Security.TrustProfile
			}
			prof := policy.TrustProfile(profStr).Normalize()
			pol := policy.ProfilePolicy(prof)
			limit = pol.RunawayGuardDefaultRounds
		}
		a.sCtx.Mu.Lock()
		a.sCtx.LoopRunawayLimit = limit
		a.sCtx.Mu.Unlock()
	}

	if budgetUSD <= 0 && th.LoopTurnBudgetUSD > 0 {
		budgetUSD = th.LoopTurnBudgetUSD
	}
	if budgetTokens <= 0 && th.LoopTurnBudgetTokens > 0 {
		budgetTokens = th.LoopTurnBudgetTokens
	}
	if budgetWallSec <= 0 && th.LoopTurnWallSeconds > 0 {
		budgetWallSec = th.LoopTurnWallSeconds
	}

	// 2. 失控保护与三维预算检查（USD、Tokens、WallSeconds）
	var exceededReason string
	var termReason string
	if limit > 0 && currentRound >= limit {
		exceededReason = fmt.Sprintf("runaway guard triggered: round %d >= limit %d", currentRound, limit)
		termReason = "guard"
	} else if budgetUSD > 0 && currentCost >= budgetUSD {
		exceededReason = fmt.Sprintf("turn budget USD exceeded: cost $%.4f >= budget $%.4f", currentCost, budgetUSD)
		termReason = "budget_usd"
	} else if budgetTokens > 0 && currentTokens >= budgetTokens {
		exceededReason = fmt.Sprintf("turn budget tokens exceeded: tokens %d >= budget %d", currentTokens, budgetTokens)
		termReason = "budget_tokens"
	} else if budgetWallSec > 0 && currentWallSec >= float64(budgetWallSec) {
		exceededReason = fmt.Sprintf("turn wall time exceeded: wall %.1fs >= budget %ds", currentWallSec, budgetWallSec)
		termReason = "budget_wall"
	}

	if exceededReason != "" {
		a.sCtx.Mu.Lock()
		warned := a.sCtx.LoopWarningInjected
		a.sCtx.Mu.Unlock()

		if !warned {
			// 首触上限：记录易变收尾提示（user 角色易变区，不追加为 system 污染历史与破坏缓存，R2-17）
			a.sCtx.Mu.Lock()
			a.sCtx.LoopWarningInjected = true
			a.sCtx.LoopWarningPrompt = "预算/失控保护告警：" + exceededReason + "，请在本轮立即总结当前进度并给出最终回复或收尾。"
			a.sCtx.Mu.Unlock()
			slog.WarnContext(ctx, "agentic_loop: budget/runaway guard warning injected", "reason", exceededReason)
		} else {
			// 次轮仍未收尾：硬熔断并保存进度
			metrics.RecordLoopTermination(ctx, termReason)
			guardErr := apperr.New(apperr.CodeResourceExhausted, exceededReason)
			a.abortTurn(ctx, guardErr)
			return guardErr
		}
	}

	// 3. call_tool 解包（ADR-0120 决策三）后构建单层批 DAGPlan，经过 S_VALIDATE 四层校验链并执行
	calls, badCalls := a.unwrapCallTools(calls)
	for _, b := range badCalls {
		a.appendToolErrorMessages([]types.InferToolCall{b.call}, "Error: "+b.msg)
	}
	if len(calls) > 0 {
		suspended, err := a.executeBatchWithDelegation(ctx, calls)
		if err != nil {
			return err
		}
		if suspended {
			return nil
		}
	}

	// 6. 执行 T3 批量观察遮蔽
	a.applyLoopObservationMasking(ctx)

	// 7. 消费插话队列（Steer），在轮次边界注入为 user 消息（R4-05）
	a.sCtx.DrainSteerQueueToMessages()

	// 8. 记录指标与持久化增量快照（P1 / ADR-0104 / HE-6）
	metrics.RecordLoopRound(ctx)
	a.persistLoopRoundDelta(ctx)

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
		toolName := c.Name
		if toolName == "code_act" {
			var codeArgs struct {
				Language string `json:"language"`
			}
			lang := "python"
			if err := json.Unmarshal(c.Input, &codeArgs); err == nil && codeArgs.Language != "" {
				lang = codeArgs.Language
			}
			toolName = "code_act:" + lang
		}
		nodes[i] = protocol.ExecNode{
			ID:         id,
			ToolName:   toolName,
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
	for _, c := range calls {
		a.appendLoopMessages(types.Message{
			Role:    "user",
			Content: errText,
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

// SetLoopCompletion 设置当前/下一回合的循环完成判定模式（"auto" | "explicit"）（R3-07 / R4-04）。
func (a *Agent) SetLoopCompletion(mode string) {
	if a == nil || a.sCtx == nil {
		return
	}
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()
	a.sCtx.LoopCompletion = mode
	a.sCtx.PendingLoopCompletion = mode
}

// runLoopRecall 执行 agentic 循环首轮确定性记忆召回与用户首条消息装配（R4-02）。
// 召回在 FSM 锁外执行（满足 make fsm-io-check）。
func (a *Agent) runLoopRecall(ctx context.Context) error {
	a.sCtx.Mu.Lock()
	intentText := a.sCtx.RawIntentTS.UnsafeContent()
	if a.sCtx.TaskID == "" {
		a.sCtx.TaskID = a.sCtx.SessionID
	}
	a.sCtx.Mu.Unlock()

	var recallText string
	var recallTaint types.TaintLevel
	var recallErr error
	if a.memory != nil && intentText != "" && !trivialIntent(intentText) {
		recallText, recallTaint, recallErr = agentctx.TurnRecallText(
			ctx,
			a.memory,
			a.sCtx.Cognitive,
			a.sCtx,
			agentctx.RecallWant{
				EpisodicQuery: intentText,
				EpisodicK:     agentctx.PerceiveEpisodicK,
				// Goal 非空才会查 反思 / L2 / RAG 知识库；缺了它默认模式就用不上知识库（R5-03）。
				Goal: intentText,
			},
			"AgenticLoopRecall",
		)
	}

	recallText = capRecallText(recallText, config.CurrentThresholds().M4Kernel.LoopRecallMaxChars)

	if recallErr != nil {
		slog.WarnContext(ctx, "agentic_loop: recall failed, degrading to no recall", "err", recallErr)
		metrics.RecordLoopRecallFailure(ctx)
		a.sCtx.Mu.Lock()
		a.sCtx.TurnRecall = nil
		a.sCtx.Mu.Unlock()
		recallText = ""
		recallTaint = types.TaintNone
	}

	volatile := a.turnVolatileContent()
	a.sCtx.Mu.Lock()
	if a.sCtx.SessionLayerSnapshot == nil && a.personaRefiner != nil {
		a.sCtx.SessionPreferencesBlock = a.personaRefiner.PreferencesBlock()
	}
	content := a.turnOpeningContentLocked(volatile, recallText)
	msgTaint := a.sCtx.RawIntentTS.Level()
	if recallText != "" {
		// 与 staged 的 writePlanRecall 同口径：召回非空即至少 TaintMedium，
		// 整条消息的污点取 max(用户意图, Medium, 全局, 已装入召回项)（R5-03）。
		msgTaint = types.PropagateTaint(msgTaint, types.TaintMedium, a.sCtx.GlobalTaintLevel, recallTaint)
	}
	a.sCtx.Mu.Unlock()

	if recallText != "" {
		a.elevateGlobalTaintLevel(types.TaintMedium)
	}
	if recallTaint > types.TaintNone {
		a.elevateGlobalTaintLevel(recallTaint)
	}

	if content != "" {
		a.appendLoopMessages(types.Message{
			Role:       "user",
			Content:    content,
			TaintLevel: msgTaint,
			Round:      1,
		})
	}

	a.asyncIntent(types.TriggerLoopRecallDone)
	return nil
}

// turnOpeningContentLocked 组装回合首条 user 消息：回合级易变块 + 召回 + 围栏后的用户意图。调用方持有 sCtx.Mu。
//
// 回合级易变块（日期、扩展连接状态、本回合 ambient 技能）放在这里而不是每次调用的末尾：它在回合内
// 位置固定，从第二次调用起按命中价计费；放在末尾则每次调用都按未命中重发（ADR-0120 决策二第 5 条）。
// 抽出为纯函数是为了让前缀稳定门控（prefix_stability_test.go）与生产走同一条组装路径。
func (a *Agent) turnOpeningContentLocked(volatile, recallText string) string {
	content := taint.Spotlighting(a.sCtx.RawIntentTS)
	if recallText != "" {
		content = "## Recalled Context\n" + recallText + "\n\n" + content
	}
	if volatile != "" {
		content = "<turn_context>\n" + volatile + "\n</turn_context>\n\n" + content
	}
	return content
}

// turnVolatileContent 读取网关本回合写入 ImmutableCore 的易变层（日期/扩展状态/ambient 技能）。
func (a *Agent) turnVolatileContent() string {
	if a.memory == nil {
		return ""
	}
	core := a.memory.ImmutableCore()
	if core == nil {
		return ""
	}
	return core.VolatileContent()
}

// trivialIntent 寒暄/短确认类意图（少于 6 个字符且不含问号）跳过自动召回：召回文本按未命中价计费，
// 对"好的""继续"这类输入没有信息增益（ADR-0120 决策四第 3 条）。
func trivialIntent(intent string) bool {
	t := strings.TrimSpace(intent)
	return utf8.RuneCountInString(t) < 6 && !strings.ContainsAny(t, "?？")
}

// capRecallText 把召回文本截断到 maxChars 个字符（0 = 不限），截断处注明。
func capRecallText(text string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	return string([]rune(text)[:maxChars]) + "\n…（召回已截断，需要更多请用 memory_search）"
}
