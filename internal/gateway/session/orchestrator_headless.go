package session

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runHeadless Headless 路径（Cron/Workflow/Webhook 一次性触发，经
// AgentPool.AcquireHeadless 获取短生命周期 Agent 实例）。
//
// [A-03 Step5] 原分散在 workflowadmin/workflow_engine.go runWorkflowStep、
// cronadmin/cron_runner.go（AcquireHeadless 前后片段）、
// channelsadmin/webhook_receive.go dispatchChannelMessage 三处几乎相同又不
// 完全一致的编排逻辑收敛于此（各自独立实现是历史代价：只有 webhook 分支接了
// Hooks.FireBefore("message.before")/Fire("message.after")/Fire("turn.stop")
// 与 TouchSession，workflow/cron 分支完全没有；SystemPromptGuard 扫描则由
// AgentPool.AcquireHeadless 自身统一覆盖，三个调用方从未各自遗漏，见
// guard.go 顶部注释）。收敛后三条路径统一获得：EnsureSession →
// session.new(首轮) → message.before 拦截 → SaveMessage(user) →
// AcquireHeadless（含 SystemPromptGuard 净化）→ SaveMessage(assistant) →
// SampleAndScoreReply → UpdateSessionTitle(首轮) → TouchSession →
// message.after → turn.stop，是本次收敛的核心价值锚点（补齐此前遗漏的
// Hook/持久化步骤，而非制造新分歧）。调用方专属 Hook 字段（如 Webhook 的
// POLARIS_USER_ID/POLARIS_CHAT_ID）经 Request.Metadata 透传，见 types.go。
//
// cron_runner.go 的 HITL（人工审批）前置检查、workflow_engine.go 的
// worktree 准备/清理等自动化专属编排逻辑不属于本函数职责，继续留在各自
// 调用方——AcquireHeadless 前置的领域决策与"跑一轮会话"是两类不同职责。
func (o *orchestrator) runHeadless(ctx context.Context, req Request, sink Sink) (*Result, error) {
	sessionID, isNewSession, err := o.resolveSessionID(req)
	if err != nil {
		return nil, err
	}
	req.SessionID = sessionID

	isFirstTurn, blockedResult, err := o.prepareHeadlessTurn(ctx, sink, req, sessionID, isNewSession)
	if err != nil {
		return nil, err
	}
	if blockedResult != nil {
		return blockedResult, nil
	}

	intent := types.Intent{Query: req.Input, WorkingDir: req.WorkingDir}
	res, err := o.agentPool.AcquireHeadless(ctx, intent)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "session.RunTurn(headless): acquire headless failed", err)
	}

	reply := o.finishHeadlessTurn(ctx, req, sessionID, isFirstTurn, res)

	// message.after / turn.stop：三条 Headless 调用方此前只有 Webhook 分支接了
	// （workflow/cron 分支完全没有），A-03 Step5 起统一触发，是本次收敛新补齐
	// 的能力而非行为收窄（ADR-0016 §2.2 Codex Stop 事件语义，对应交互式路径
	// runInteractive 同名 hook，见 orchestrator_interactive.go）。
	o.hooks.Fire("message.after", mergeHookEnv(req.Metadata, map[string]string{
		"POLARIS_REPLY":      reply,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    req.Channel,
	}))
	o.hooks.Fire("turn.stop", mergeHookEnv(req.Metadata, map[string]string{
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    req.Channel,
	}))

	if reply != "" {
		_ = sink.Emit(Event{Kind: KindDelta, Text: reply})
	}
	_ = sink.Emit(Event{Kind: KindComplete, Payload: map[string]any{
		"session_id":  sessionID,
		"duration_ms": res.LatencyMs,
	}})

	return &Result{SessionID: sessionID, Reply: reply, LatencyMs: res.LatencyMs}, nil
}

// prepareHeadlessTurn 落地会话确保 + Hook 分发 + 用户消息持久化（从 runHeadless
// 拆出，gocyclo 治理，行为不变）。blockedResult 非 nil 时调用方应直接
// return blockedResult, nil（message.before 拦截）。
func (o *orchestrator) prepareHeadlessTurn(ctx context.Context, sink Sink, req Request, sessionID string, isNewSession bool) (isFirstTurn bool, blockedResult *Result, err error) {
	if err := o.persistence.EnsureSession(ctx, sessionID); err != nil {
		return false, nil, apperr.Wrap(apperr.CodeInternal, "session.RunTurn(headless): ensure session", err)
	}
	if isNewSession {
		o.hooks.Fire("session.new", mergeHookEnv(req.Metadata, map[string]string{
			"POLARIS_SESSION_ID": sessionID,
			"POLARIS_CHANNEL":    req.Channel,
		}))
	}

	if blocked, reason := o.hooks.FireBefore("message.before", mergeHookEnv(req.Metadata, map[string]string{
		"POLARIS_MESSAGE":    req.Input,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    req.Channel,
	})); blocked {
		slog.Info("session: headless turn blocked by hook", "session", sessionID, "channel", req.Channel, "reason", reason)
		_ = sink.Emit(Event{Kind: KindError, Payload: map[string]any{"code": "hook_blocked", "message": reason}})
		return false, &Result{SessionID: sessionID, Aborted: true}, nil
	}

	history, err := o.persistence.ListMessages(ctx, sessionID)
	if err != nil {
		return false, nil, apperr.Wrap(apperr.CodeInternal, "session.RunTurn(headless): list messages", err)
	}
	isFirstTurn = len(history) == 0

	userMessage := req.Input
	if req.WorkingDir != "" {
		userMessage = "[工作目录: " + req.WorkingDir + "]\n\n" + req.Input
	}
	if err := o.persistence.SaveMessage(ctx, sessionID, "user", userMessage, "", "", 0); err != nil {
		slog.Warn("session: headless saveMessage user failed", "session", sessionID, "err", err)
	}

	return isFirstTurn, nil, nil
}

// finishHeadlessTurn 助手消息持久化 + 会话标题/TouchSession（从 runHeadless
// 拆出，gocyclo 治理，行为不变）。返回回复文本。
//
// [A-03 Step5 决策修正] 不在此处重复 SystemPromptGuard 扫描：res.Output 由
// AgentPool.AcquireHeadless（internal/agent/pool.go）返回前已扫描净化——该
// 扫描是 AcquireHeadless 自身职责的一部分（覆盖包括本路径在内的全部直接/间接
// 调用方，见 guard.go 顶部注释的决策记录），本包二次持有一份重复单例只会
// 徒增一次空扫描成本，不提供额外保护。
func (o *orchestrator) finishHeadlessTurn(ctx context.Context, req Request, sessionID string, isFirstTurn bool, res *types.AgentResult) string {
	reply := res.Output

	if reply != "" {
		if err := o.persistence.SaveMessage(ctx, sessionID, "assistant", reply, "", "", res.LatencyMs); err != nil {
			slog.Warn("session: headless saveMessage assistant failed", "session", sessionID, "err", err)
		}
	}
	o.persistence.SampleAndScoreReply(sessionID, req.Input, reply)

	if isFirstTurn {
		titleHint := req.TitleHint
		if titleHint == "" {
			titleHint = req.Input
		}
		if err := o.persistence.UpdateSessionTitle(ctx, sessionID, titleHint); err != nil {
			slog.Warn("session: headless update session title failed", "session", sessionID, "err", err)
		}
	}
	if err := o.persistence.TouchSession(ctx, sessionID); err != nil {
		slog.Warn("session: headless touch session failed", "session", sessionID, "err", err)
	}

	return reply
}
