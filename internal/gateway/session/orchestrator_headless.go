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
// channelsadmin/webhook_receive.go dispatchToAgent 三处几乎相同又不完全一致
// 的编排逻辑收敛于此（各自独立实现是历史代价：只有 webhook 分支接了
// Hooks.FireBefore("message.before") 与 TouchSession，workflow/cron 分支
// 完全没有；SystemPromptGuard 扫描此前只在 pool.go 内部单独接了一次，
// workflow/cron/webhook 三个调用方都不知情）。收敛后三条路径统一获得：
// EnsureSession → session.new(首轮) → message.before 拦截 → SaveMessage(user)
// → AcquireHeadless → SystemPromptGuard 扫描 → SaveMessage(assistant) →
// SampleAndScoreReply → UpdateSessionTitle(首轮) → TouchSession，是本次收敛
// 的核心价值锚点（补齐此前遗漏的防护与持久化步骤，而非制造新分歧）。
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
		o.hooks.Fire("session.new", map[string]string{
			"POLARIS_SESSION_ID": sessionID,
			"POLARIS_CHANNEL":    req.Channel,
		})
	}

	if blocked, reason := o.hooks.FireBefore("message.before", map[string]string{
		"POLARIS_MESSAGE":    req.Input,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    req.Channel,
	}); blocked {
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

// finishHeadlessTurn SystemPromptGuard 扫描 + 助手消息持久化 + 会话标题/
// TouchSession（从 runHeadless 拆出，gocyclo 治理，行为不变）。返回净化后的
// 回复文本。
func (o *orchestrator) finishHeadlessTurn(ctx context.Context, req Request, sessionID string, isFirstTurn bool, res *types.AgentResult) string {
	reply := res.Output
	// [W-2-B] SystemPromptGuard：headless 路径一次性扫描完整输出（拼接完成的
	// 全量文本，无需处理跨 chunk 边界问题），redact=true 净化后继续返回，
	// 不中断 Cron/Workflow/Webhook 自动化。原 internal/agent/pool.go
	// AcquireHeadless 内联逻辑迁入本包统一收口（见本文件与 guard.go 顶部注释）。
	if cleaned, scanErr := headlessPromptGuard().Scan(reply, true); scanErr == nil {
		reply = cleaned
	} else {
		slog.Warn("session: system prompt guard scan failed on headless output", "session_id", sessionID, "err", scanErr)
	}

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
