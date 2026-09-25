package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/pkg/types"
)

// turnPhaseOf 把正在执行 Effect 的 FSM 状态映射为对外阶段键（ADR-0098 决策一）。
// 非回合阶段（Idle/Validate/Replan/终态等）返回空串，不发布。
func turnPhaseOf(s types.AgentState) types.TurnPhase {
	switch s {
	case types.AgentStatePerceive:
		return types.TurnPhasePerceive
	case types.AgentStatePlan:
		return types.TurnPhasePlan
	case types.AgentStateExecute:
		return types.TurnPhaseExecute
	case types.AgentStateReflect:
		return types.TurnPhaseReflect
	case types.AgentStateRespond:
		return types.TurnPhaseRespond
	default:
		return ""
	}
}

// llmPurposeOf 把发起 LLM 调用时的 FSM 状态映射为 llm_calls.purpose（ADR-0101 决策六）。
// S_VALIDATE 的 L3 看门狗不是回合阶段，单独命名。
func llmPurposeOf(s types.AgentState) string {
	if s == types.AgentStateValidate {
		return "validate_watchdog"
	}
	if phase := turnPhaseOf(s); phase != "" {
		return string(phase)
	}
	return "kernel"
}

// publishTurnPhase 内部阶段不再推 token 后，用户在首个回复 token 前只能靠阶段
// 事件感知进度；否则一次"规划 + 执行"的回合在界面上是长时间的空白。
func (a *Agent) publishTurnPhase(s types.AgentState) {
	phase := turnPhaseOf(s)
	if phase == "" {
		return
	}
	a.publishStreamEvent(types.AgentStreamEvent{
		Type:    types.AgentStreamEventPhase,
		Content: string(phase),
	})
}

// publishPreparedReply 发布 Perceive 同次产出的直答（ADR-0102 决策四 4b′）并消费之。
// 事件形态与 doStreamInfer 的 AudienceUser 文本增量一致，session 侧无需区分来源。
func (a *Agent) publishPreparedReply() {
	a.sCtx.Mu.Lock()
	reply := a.sCtx.PreparedReply
	a.sCtx.PreparedReply = ""
	taintLevel := a.sCtx.GlobalTaintLevel
	a.sCtx.Mu.Unlock()
	a.publishTurnPhase(types.AgentStateRespond)
	if reply == "" {
		return
	}
	a.publishStreamEvent(types.AgentStreamEvent{
		Type:       types.AgentStreamEventToken,
		Content:    reply,
		TaintLevel: taintLevel,
	})
}

// fastPathPlanState System-1 FastPath 旁路 S_PLAN 时的下一状态：没有可复用的 DAG
// 就没有可执行的东西，直接转直答，而不是空跑 Validate/Execute/Reflect 三步。
func (a *Agent) fastPathPlanState() types.State {
	if a.sCtx.DAGModel == nil || len(a.sCtx.DAGModel.Nodes) == 0 {
		return "S_PLAN_EMPTY"
	}
	return "S_PLAN_DONE"
}

// SetConversationHistory 注入本轮之前的对话历史（ADR-0098 决策四）。
// 与 SetTaskIntent 同一调用方、同一时序（SendIntent 之前），此时 FSM 空闲，
// 但仍按 sCtx.Mu 写入：Prompt 构建路径以 RLock 读取该字段。
// 剔除 system 角色：session 历史首条是注入的系统提示词，内核有自己的
// ImmutableCore，重复携带既浪费预算又会让两份人格指令互相竞争。
func (a *Agent) SetConversationHistory(history []types.Message) {
	kept := make([]types.Message, 0, len(history))
	for _, m := range history {
		if m.Role == "system" {
			continue
		}
		kept = append(kept, types.Message{Role: m.Role, Content: m.Content})
	}
	a.sCtx.Mu.Lock()
	a.sCtx.ConversationHistory = kept
	a.sCtx.Mu.Unlock()
}

// reportUnusableFill 处理"推理成功、但产出不可用"（OnSuccess 返回错误）的情形。
//
// 推理失败已在 executeEffect 里落日志并发错误事件；这条路径此前两样都没有：
// 错误被 nextState 吞掉，FSM 静默转 S_FAILED，用户只看到 session 层兜底的
// "推理返回空内容"，日志里也无从区分"模型只输出了思考链""输出被截断""格式错误"
// （2026-09-25 实测 S_RESPOND 空正文即此）。
func (a *Agent) reportUnusableFill(ctx context.Context, next types.State, err error, resp *types.ProviderResponse) {
	if err == nil {
		return
	}
	var reasoningLen, contentLen, outTokens int
	if resp != nil {
		reasoningLen, contentLen, outTokens = len(resp.ReasoningContent), len(resp.Content), resp.Usage.OutputTokens
	}
	slog.WarnContext(ctx, "kernel: llm fill unusable",
		"agent_id", a.ID, "session", a.sCtx.SessionID, "state", a.sm.Current(), "next", next,
		"content_bytes", contentLen, "reasoning_bytes", reasoningLen, "output_tokens", outTokens, "err", err)
	if stateToTriggerMap()[next] != types.TriggerReplanExhausted {
		return // 非终止性：OnSuccess 已按降级语义继续推进，只需留痕
	}
	a.publishStreamEvent(types.AgentStreamEvent{
		Type:       types.AgentStreamEventError,
		Content:    "模型输出不可用：" + err.Error(),
		TaintLevel: a.sCtx.GlobalTaintLevel,
	})
}

// abortTurn 回合因内核错误中止时的唯一出口（ADR-0098 回合终止语义）。
//
// 此前 Effect 错误只发错误事件、Dispatch 错误（含 ReplanExhausted、转移表缺口）
// 什么都不发，Run() 直接返回：订阅通道只在订阅 ctx 结束时才关，会话等不到
// task_done 一直挂到客户端超时（2026-09-25 实测 300s）；且 FSM 停在非终态，
// Pool 只对 Complete/Failed 换新实例，下一轮 SendIntent 投给已无 Run() 消费的
// 旧实例。强制进 S_FAILED 并走 handleTerminalState，三个问题一并收口。
func (a *Agent) abortTurn(ctx context.Context, err error) {
	slog.WarnContext(ctx, "kernel: turn aborted", "agent_id", a.ID, "session", a.sCtx.SessionID,
		"state", a.sm.Current(), "err", err)
	a.publishStreamEvent(types.AgentStreamEvent{
		Type:       types.AgentStreamEventError,
		Content:    "Agent 执行失败：" + err.Error(),
		TaintLevel: a.sCtx.GlobalTaintLevel,
	})
	if cur := a.sm.Current(); cur != types.AgentStateFailed && cur != types.AgentStateComplete {
		a.sm.ForceState(types.AgentStateFailed)
	}
	a.handleTerminalState(ctx, types.AgentStateFailed)
}

// validationFailureKind S_VALIDATE 拒绝的成因（ADR-0102 决策六）：只有 L0 结构错误计入升级。
// 非 DAGValidationError（如 L1-Taint 包装错误、校验器缺失）按策略拒绝处理——升级模型不改变结论。
func validationFailureKind(err error) fsm.FailureKind {
	var ve *protocol.DAGValidationError
	if errors.As(err, &ve) {
		return fsm.ClassifyValidationLayer(ve.Layer)
	}
	return fsm.FailurePolicy
}

// executionFailureKind S_EXECUTE 失败的成因：瞬时/环境类（超时、网络、限流、取消、Provider 耗尽）
// 不升级；其余视为工具报错（参数错、对象不存在等），重复出现才升级。
func executionFailureKind(err error) fsm.FailureKind {
	for _, c := range []apperr.Code{apperr.CodeTimeout, apperr.CodeCancelled, apperr.CodeNetworkUnavailable,
		apperr.CodeResourceExhausted, apperr.CodeProviderExhausted, apperr.CodeStorageUnavailable, apperr.CodeConflict} {
		if apperr.IsCode(err, c) {
			return fsm.FailureTransient
		}
	}
	return fsm.FailureToolError
}

// validationFeedback 把 S_VALIDATE 的结构化拒绝翻译成"哪个工具、被哪层、为何拒绝"，
// 供重规划回灌（ADR-0098 决策六）。节点 ID 对模型无意义（call_xx 每次重生成），
// 必须换成工具名，模型才知道该避开什么。
func validationFeedback(plan *protocol.DAGPlan, err error) string {
	var ve *protocol.DAGValidationError
	if !errors.As(err, &ve) {
		return "plan rejected by validator: " + err.Error()
	}
	tool := ""
	if plan != nil {
		for _, n := range plan.Nodes {
			if n.ID == ve.NodeID {
				tool = n.ToolName
				break
			}
		}
	}
	if tool == "" {
		return fmt.Sprintf("plan rejected by %s: %s", ve.Layer, ve.Reason)
	}
	return fmt.Sprintf("plan rejected by %s for tool %q: %s", ve.Layer, tool, ve.Reason)
}
