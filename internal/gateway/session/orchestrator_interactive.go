package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// runInteractive 交互式路径（SSE 入口，per-session 长驻 Agent 实例）。
//
// [A-03 Step2] 原 chat/sse.go HandleAgentStream + handleAgentStreamFSM +
// chat/sse_stream_helpers.go acquireStreamAgent/handleStreamFSMEvent 逐段
// 搬入本函数（纯搬运，行为等价），w/flusher 写入统一替换为 sink.Emit。
// HTTP 特有部分（请求解码/响应头设置/Flusher 校验/S-07 早期白名单）保留在
// HTTP 边界层（chat/sse.go 瘦身后的 HandleAgentStream，见 A-03 Step4）。
//
//nolint:gocyclo,funlen // 原 HandleAgentStream 整体 nolint:gocyclo 覆盖范围内的既有复杂度，迁移未新增分支
func (o *orchestrator) runInteractive(ctx context.Context, req Request, sink Sink) (*Result, error) {
	// @file/@url/git: 引用展开（消息预处理入口）：ContextRefExpander 未注入时
	// 原样返回 input；单条引用展开失败记入 skipped（已在 PromptAssembler 适配
	// 方法内 slog.Warn）但不阻断请求。
	req.Input, _ = o.prompt.ExpandContextRefs(ctx, req.Input)

	sessionID, isNewSession, err := o.resolveSessionID(req)
	if err != nil {
		return nil, err
	}
	req.SessionID = sessionID

	if err := o.persistence.EnsureSession(ctx, sessionID); err != nil {
		o.emitError(sink, "session_error", err.Error(), sessionID, err)
		return &Result{SessionID: sessionID, Aborted: true}, nil
	}
	// session.new hook：用户发起新会话时触发（req.SessionID 为空意味着 /new 后首条消息）
	if isNewSession {
		o.hooks.Fire("session.new", map[string]string{
			"POLARIS_SESSION_ID": sessionID,
			"POLARIS_CHANNEL":    req.Channel,
		})
	}

	// message.before hook：同步拦截，非零退出 = 拒绝本条消息
	if blocked, reason := o.hooks.FireBefore("message.before", map[string]string{
		"POLARIS_MESSAGE":    req.Input,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    req.Channel,
	}); blocked {
		o.emitError(sink, "hook_blocked", reason, sessionID, nil)
		return &Result{SessionID: sessionID, Aborted: true}, nil
	}

	// 加载历史消息（多轮上下文）
	history, err := o.persistence.ListMessages(ctx, sessionID)
	if err != nil {
		o.emitError(sink, "history_error", err.Error(), sessionID, err)
		return &Result{SessionID: sessionID, Aborted: true}, nil
	}
	isFirstTurn := len(history) == 0

	var agentCtrl protocol.AgentController
	var release func()
	if o.agentPool != nil {
		var acquireOK bool
		agentCtrl, release, acquireOK = o.acquireInteractiveAgent(ctx, sink, req, sessionID)
		if !acquireOK {
			return &Result{SessionID: sessionID, Aborted: true}, nil
		}
		defer release()
	}

	history = o.prompt.InjectSystemPrompt(ctx, agentCtrl, history, req.Input)
	// 注意：FSM 触发（SetTaskIntent/SendIntent）在 runFSMTurn 中——在订阅事件流
	// 之后执行——先订阅后触发消除早期 token 丢失竞态；且触发点位于斜杠命令
	// 短路之后，/compact 等命令不再空耗一次 FSM 推理。

	// ── Transcript ────────────────────────────────────────────────────────
	tw, twErr := openTranscript(o.transcriptDir, sessionID, isFirstTurn)
	if twErr != nil {
		slog.Warn("session: transcript open failed", "session", sessionID, "err", twErr)
	}
	if tw != nil {
		defer tw.Close()
	}

	// 追加本轮用户消息（含图片 Parts）
	finalInput, userMsg := o.buildUserMessage(req)

	history = append(history, userMsg)
	if err := o.persistence.SaveMessage(ctx, sessionID, "user", finalInput, "", "", 0); err != nil {
		slog.Error("session: saveMessage user", "session", sessionID, "err", err)
	}
	if tw != nil {
		tw.WriteTurn("user", req.Input, 0, 0)
	}

	// ── 选取最优 Provider ─────────────────────────────────────────────────
	var p protocol.Provider
	if req.ModelID != "" {
		p = o.registry.PickProviderByRecordID(req.ModelID)
	}
	if p == nil {
		p = o.registry.PickProvider("default")
		if p == nil {
			p = o.registry.PickProvider("general")
		}
	}
	if p == nil {
		if tw != nil {
			tw.WriteError("no_provider", "未配置任何启用的 LLM 厂商")
		}
		o.emitError(sink, "no_provider", "未配置任何启用的 LLM 厂商，请在「模型」页添加并启用厂商", sessionID, nil)
		return &Result{SessionID: sessionID, Aborted: true}, nil
	}

	// ── 斜线命令拦截（短路 LLM 推理）────────────────────────────────────────
	var slashMem MemoryFacade
	if agentCtrl != nil {
		if mf := agentCtrl.Memory(); mf != nil {
			slashMem = mf
		}
	}
	if res, handled := o.tryDispatchSlash(ctx, sink, sessionID, finalInput, history, p, slashMem); handled {
		return res, nil
	}

	// ── 上下文使用率评估（警告 + 防抖动告警 + 自动压缩）────────────────────────
	ctxStats := o.compression.Stats(history)

	if ctxStats.UsagePercent >= o.compression.WarnPct() {
		msg := fmt.Sprintf("上下文使用量已达 %d%%，可使用 /compact 手动压缩", int(ctxStats.UsagePercent))
		if ctxStats.Thrashing {
			msg = fmt.Sprintf("⚠ 自动压缩抖动：上下文 %d%% 使用量居高不下，请手动 /compact 并缩减单次工具输出规模", int(ctxStats.UsagePercent))
		}
		_ = sink.Emit(Event{Kind: KindContextWarning, Payload: map[string]any{
			"usage_percent": int(ctxStats.UsagePercent),
			"token_count":   ctxStats.TokenCount,
			"threshold":     ctxStats.Threshold,
			"thrashing":     ctxStats.Thrashing,
			"message":       msg,
		}})
	}

	// 自动压缩：非 thrashing 状态 + 超过 autoCompactPct 阈值 → 静默压缩后继续推理
	if !ctxStats.Thrashing && o.compression.NeedsCompact(history) {
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{"type": "compacting", "message": "正在压缩上下文..."}})

		var mem MemoryFacade
		if agentCtrl != nil {
			mem = agentCtrl.Memory()
		}

		if _, res, err := o.compression.Compact(ctx, sessionID, history, p, mem); err == nil && !res.Skipped {
			_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{
				"type":          "compacted",
				"tokens_before": res.TokensBefore,
				"tokens_after":  res.TokensAfter,
				"message":       fmt.Sprintf("上下文已压缩：%d → %d tokens", res.TokensBefore, res.TokensAfter),
			}})
		}
	}

	inferStart := time.Now()

	var reply string
	var inferErr string
	var aborted bool

	if agentCtrl == nil {
		o.emitError(sink, "no_agent", "系统错误：未找到当前会话的 Agent 控制器", sessionID, nil)
		return &Result{SessionID: sessionID, Aborted: true}, nil
	}
	reply, inferErr, aborted = o.runFSMTurn(ctx, sink, sessionID, agentCtrl, req.Input)
	if aborted {
		// GD-13-004 部分缓解：客户端断连/中止时不再静默丢弃已产出的部分回复。
		if reply != "" {
			saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := o.persistence.SaveMessage(saveCtx, sessionID, "assistant", reply, "", "", 0); err != nil {
				slog.Error("session: saveMessage assistant (aborted turn)", "session", sessionID, "err", err)
			}
			saveCancel()
		}
		return &Result{SessionID: sessionID, Reply: reply, Aborted: true}, nil
	}
	inferLatencyMs := time.Since(inferStart).Milliseconds()

	// 推理成功返回但无内容（超时/内容过滤/空响应）
	if reply == "" && inferErr == "" {
		inferErr = "推理返回空内容，请检查模型配置或重试"
	}
	if inferErr != "" {
		if tw != nil {
			tw.WriteError("empty_response", inferErr)
		}
		o.emitError(sink, "empty_response", inferErr, sessionID, apperr.New(apperr.CodeInternal, "log event"))
		return &Result{SessionID: sessionID, Aborted: true}, nil
	}

	// ── 持久化 assistant 回复 ─────────────────────────────────────────────
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()

	if reply != "" {
		if err := o.persistence.SaveMessage(saveCtx, sessionID, "assistant", reply, "", "", inferLatencyMs); err != nil {
			slog.Error("session: saveMessage assistant", "session", sessionID, "err", err)
		}
		o.persistence.SampleAndScoreReply(sessionID, req.Input, reply)
		if tw != nil {
			tw.WriteTurn("assistant", reply, inferLatencyMs, 0)
		}
	}
	if isFirstTurn {
		titleHint := req.TitleHint
		if titleHint == "" {
			titleHint = req.Input
		}
		if err := o.persistence.UpdateSessionTitle(saveCtx, sessionID, titleHint); err != nil {
			slog.Warn("session: update session title failed", "session", sessionID, "err", err)
		}
	}
	if err := o.persistence.TouchSession(saveCtx, sessionID); err != nil {
		slog.Warn("session: touch session failed", "session", sessionID, "err", err)
	}

	slog.Info("session: turn complete",
		"session", sessionID,
		"latency_ms", inferLatencyMs,
		"reply_bytes", len(reply),
		"client_cancelled", ctx.Err() != nil,
	)

	// message.after hook：fire-and-forget，不阻塞响应
	o.hooks.Fire("message.after", map[string]string{
		"POLARIS_REPLY":      reply,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    req.Channel,
	})
	// turn.stop hook（对应 ADR-0016 §2.2 Codex Stop 事件语义）
	o.hooks.Fire("turn.stop", map[string]string{
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    req.Channel,
	})

	_ = sink.Emit(Event{Kind: KindComplete, Payload: map[string]any{
		"session_id":  sessionID,
		"duration_ms": inferLatencyMs,
	}})

	return &Result{SessionID: sessionID, Reply: reply, LatencyMs: inferLatencyMs}, nil
}

// tryDispatchSlash 斜线命令短路子步骤（从 runInteractive 拆出，nestif 治理，
// 行为不变）。handled=true 时调用方应直接 return res, nil；o.slash 为 nil
// （未注入路由器）或命令未被处理时 handled=false，res 为 nil。
func (o *orchestrator) tryDispatchSlash(
	ctx context.Context,
	sink Sink,
	sessionID, finalInput string,
	history []types.Message,
	p protocol.Provider,
	slashMem MemoryFacade,
) (res *Result, handled bool) {
	if o.slash == nil {
		return nil, false
	}
	cmdResult := o.slash.Dispatch(ctx, finalInput, sessionID, history, p, sink, slashMem)
	if !cmdResult.Handled {
		return nil, false
	}
	if cmdResult.Response != "" {
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := o.persistence.SaveMessage(saveCtx, sessionID, "assistant", cmdResult.Response, "", "", 0); err != nil {
			slog.Error("session: saveMessage slash response", "session", sessionID, "err", err)
		}
		cancel()
	}
	if err := o.persistence.TouchSession(context.WithoutCancel(ctx), sessionID); err != nil {
		slog.Warn("session: touch session failed (slash command path)", "session", sessionID, "err", err)
	}
	_ = sink.Emit(Event{Kind: KindComplete, Payload: map[string]any{"session_id": sessionID, "session_title": ""}})
	return &Result{SessionID: sessionID, SlashHandled: true}, true
}

// acquireInteractiveAgent 从 AgentPool 获取本次流式对话的 AgentController，
// 处理资源耗尽降级（原 chat/sse_stream_helpers.go acquireStreamAgent 迁入，
// 行为不变）。ok=false 表示已经推送错误/降级提示，调用方应立即 return；
// release 仅在 ok=true 时有效。
func (o *orchestrator) acquireInteractiveAgent(ctx context.Context, sink Sink, req Request, sessionID string) (protocol.AgentController, func(), bool) {
	agentCtrl, release, err := o.agentPool.Acquire(ctx, sessionID)
	if err == nil {
		return agentCtrl, release, true
	}

	var aerr *apperr.Error
	if errors.As(err, &aerr) && aerr.Code == apperr.CodeResourceExhausted {
		// 后台计算请求
		if req.RunID != "" || req.ReasoningEffort == "background" {
			o.emitError(sink, "system_notice", "后台提炼排队中", sessionID, nil)
			return nil, nil, false
		}
		// 前台对话请求：非错误性状态提示（保留独立 wire 事件名 "system_notice"，
		// 不经 emitError——原实现不记录日志，仅推送提示）。
		_ = sink.Emit(Event{Kind: KindSystemNotice, Payload: map[string]any{
			"message": "系统当前负载较高，已为您转入沙箱保护模式，稍等片刻",
			"retry":   true,
		}})
		return nil, nil, false
	}
	o.emitError(sink, "agent_pool_error", "failed to acquire agent: "+err.Error(), sessionID, err)
	return nil, nil, false
}
