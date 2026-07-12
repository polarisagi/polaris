package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// skillEmbedCacheMax 技能文本→向量缓存上限：超限时随机淘汰（技能数量有限，
// 随机淘汰成本低）。容量估算：512 × 1536 维 × 4 字节 ≈ 3MB，可接受。
const skillEmbedCacheMax = 512

// handleAgentStream 处理 SSE 方式的流式对话。
// 将用户输入包装后转发给 Agent FSM，并订阅 FSM 产生的事件流推送到客户端。
func (s *ChatHandler) HandleAgentStream(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo
	var req agentStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Attachments) == 0 && len(req.ImageParts) == 0 {
		http.Error(w, "input required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关闭 nginx 缓冲

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// SSE 长连接：禁用写超时
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	ctx := r.Context()

	// @file/@url/git 引用展开（消息预处理入口）：ContextRefExpander 为 nil 时
	// 跳过（未注入场景，行为与展开前完全一致）；展开失败的单条引用会被记录到
	// report.Skipped 但不阻断请求，避免因单个引用问题导致整轮对话失败。
	if s.ContextRefExpander != nil {
		if expanded, report := s.ContextRefExpander.Expand(ctx, req.Input); report != nil {
			req.Input = expanded
			if len(report.Skipped) > 0 {
				slog.Warn("server: context ref expand skipped some references", "skipped", report.Skipped)
			}
		}
	}

	// ── 会话管理 ──────────────────────────────────────────────────────────
	sessionID := strings.TrimSpace(req.SessionID)
	isNewSession := sessionID == ""
	if isNewSession {
		sessionID = newSessionID()
	}
	if err := s.EnsureSession(ctx, sessionID); err != nil {
		s.WriteSSEError(w, flusher, "session_error", err.Error(), sessionID, err)
		return
	}
	// session.new hook：用户发起新会话时触发（req.SessionID 为空意味着 /new 后首条消息）
	if isNewSession {
		s.Hooks.Fire("session.new", map[string]string{
			"POLARIS_SESSION_ID": sessionID,
			"POLARIS_CHANNEL":    "web",
		})
	}

	// message.before hook：同步拦截，非零退出 = 拒绝本条消息
	if blocked, reason := s.Hooks.FireBefore("message.before", map[string]string{
		"POLARIS_MESSAGE":    req.Input,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    "web",
	}); blocked {
		s.WriteSSEError(w, flusher, "hook_blocked", reason, sessionID, nil)
		return
	}

	// 加载历史消息（多轮上下文）
	history, err := s.ListMessages(ctx, sessionID)
	if err != nil {
		s.WriteSSEError(w, flusher, "history_error", err.Error(), sessionID, err)
		return
	}
	isFirstTurn := len(history) == 0

	var agentCtrl protocol.AgentController
	if s.AgentPool != nil {
		var release func()
		var err error
		agentCtrl, release, err = s.AgentPool.Acquire(ctx, sessionID)
		if err != nil {
			s.WriteSSEError(w, flusher, "agent_pool_error", "failed to acquire agent: "+err.Error(), sessionID, err)
			return
		}
		defer release()
	}

	history = s.InjectSystemPrompt(ctx, agentCtrl, history, req.Input)
	// 注意：FSM 触发（SetTaskIntent/SendIntent）已移入 handleAgentStreamFSM，
	// 在订阅事件流之后执行——先订阅后触发消除早期 token 丢失竞态；
	// 且触发点位于斜杠命令短路之后，/compact 等命令不再空耗一次 FSM 推理。

	// ── Transcript ────────────────────────────────────────────────────────
	// 非阻塞：打开失败只警告，不中断对话。
	tw, twErr := openTranscript(s.TranscriptDir, sessionID, isFirstTurn)
	if twErr != nil {
		slog.Warn("server: transcript open failed", "session", sessionID, "err", twErr)
	}
	if tw != nil {
		defer tw.Close()
	}

	// 追加本轮用户消息（含图片 Parts）
	finalInput, userMsg := s.buildStreamUserMessage(req)

	history = append(history, userMsg)
	if err := s.SaveMessage(ctx, sessionID, "user", finalInput, "", "", 0); err != nil {
		slog.Error("server: saveMessage user", "session", sessionID, "err", err)
	}
	if tw != nil {
		tw.WriteTurn("user", req.Input, 0, 0)
	}

	// ── 选取最优 Provider ─────────────────────────────────────────────────
	var p protocol.Provider
	if req.ModelID != "" {
		p = s.Registry.PickProviderByRecordID(req.ModelID)
	}
	if p == nil {
		// 优先用 "default" 角色（对话模型），次选 "general"（参与全局 LB）
		p = s.Registry.PickProvider("default")
		if p == nil {
			p = s.Registry.PickProvider("general")
		}
	}
	if p == nil {
		if tw != nil {
			tw.WriteError("no_provider", "未配置任何启用的 LLM 厂商")
		}
		s.WriteSSEError(w, flusher, "no_provider", "未配置任何启用的 LLM 厂商，请在「模型」页添加并启用厂商", sessionID, nil)
		return
	}

	// ── 斜线命令拦截（短路 LLM 推理）────────────────────────────────────────
	// /compact 走与自动压缩相同的 Stage 3 TaskMermaidCanvas 注入路径（M05 §11.3）：
	// agentCtrl 此处已解析（105 行），避免 SlashCommandRouter 构造期无法获取 per-session
	// memory facade 的问题。
	var slashMem MemoryFacade
	if agentCtrl != nil {
		if mf := agentCtrl.Memory(); mf != nil {
			slashMem = mf
		}
	}
	if cmdResult := s.SlashRouter.Dispatch(ctx, finalInput, sessionID, history, p, w, flusher, slashMem); cmdResult.Handled {
		if cmdResult.Response != "" {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.SaveMessage(saveCtx, sessionID, "assistant", cmdResult.Response, "", "", 0); err != nil {
				slog.Error("server: saveMessage slash response", "session", sessionID, "err", err)
			}
		}
		_ = s.TouchSession(context.WithoutCancel(ctx), sessionID)
		WriteSSE(w, flusher, "complete", map[string]any{"session_id": sessionID, "session_title": ""})
		return
	}

	// ── 上下文使用率评估（警告 + 防抖动告警 + 自动压缩）────────────────────────
	// 阈值模型对齐 Claude Code：contextWindow × autoCompactPct%（默认 95%），
	// warnPct%（默认 80%）时提前告警，连续 thrashing 后停止自动压缩并专项提示。
	ctxStats := s.Compressor.Stats(history)

	if ctxStats.UsagePercent >= s.Compressor.WarnPct() {
		msg := fmt.Sprintf("上下文使用量已达 %d%%，可使用 /compact 手动压缩", int(ctxStats.UsagePercent))
		if ctxStats.Thrashing {
			msg = fmt.Sprintf("⚠ 自动压缩抖动：上下文 %d%% 使用量居高不下，请手动 /compact 并缩减单次工具输出规模", int(ctxStats.UsagePercent))
		}
		WriteSSE(w, flusher, "context_warning", map[string]any{
			"usage_percent": int(ctxStats.UsagePercent),
			"token_count":   ctxStats.TokenCount,
			"threshold":     ctxStats.Threshold,
			"thrashing":     ctxStats.Thrashing,
			"message":       msg,
		})
	}

	// 自动压缩：非 thrashing 状态 + 超过 autoCompactPct 阈值 → 静默压缩后继续推理
	if !ctxStats.Thrashing && s.Compressor.NeedsCompact(history) {
		WriteSSE(w, flusher, "status", map[string]any{"type": "compacting", "message": "正在压缩上下文..."})

		var mem protocol.MemoryFacade
		if agentCtrl != nil {
			mem = agentCtrl.Memory()
		}

		if compacted, res, err := s.Compressor.Compact(ctx, sessionID, history, p, mem); err == nil && !res.Skipped {
			history = compacted
			WriteSSE(w, flusher, "status", map[string]any{
				"type":          "compacted",
				"tokens_before": res.TokensBefore,
				"tokens_after":  res.TokensAfter,
				"message":       fmt.Sprintf("上下文已压缩：%d → %d tokens", res.TokensBefore, res.TokensAfter),
			})
		}
	}

	inferStart := time.Now()

	var reply string
	var inferErr string
	var aborted bool

	if agentCtrl == nil {
		s.WriteSSEError(w, flusher, "no_agent", "系统错误：未找到当前会话的 Agent 控制器", sessionID, nil)
		return
	}
	reply, inferErr, aborted = s.handleAgentStreamFSM(ctx, w, flusher, sessionID, agentCtrl, req.Input)
	if aborted {
		// GD-13-004 部分缓解：客户端断连/中止时不再静默丢弃已产出的部分回复。
		// 原实现直接 return，reply 中已流式产出的 assistant 内容从未落盘，
		// 造成 UI 侧 chat 历史与 Agent 侧 EventLog 记忆流不同步（"幽灵消息"）。
		// 这里尽力保存已产出内容（若有），ctx 已取消，用独立的短超时 context。
		if reply != "" {
			saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.SaveMessage(saveCtx, sessionID, "assistant", reply, "", "", 0); err != nil {
				slog.Error("server: saveMessage assistant (aborted turn)", "session", sessionID, "err", err)
			}
			saveCancel()
		}
		return
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
		s.WriteSSEError(w, flusher, "empty_response", inferErr, sessionID, apperr.New(apperr.CodeInternal, "log event"))
		return
	}

	// ── 持久化 assistant 回复 ─────────────────────────────────────────────
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()

	if reply != "" {
		if err := s.SaveMessage(saveCtx, sessionID, "assistant", reply, "", "", inferLatencyMs); err != nil {
			slog.Error("server: saveMessage assistant", "session", sessionID, "err", err)
		}
		if tw != nil {
			tw.WriteTurn("assistant", reply, inferLatencyMs, 0)
		}
	}
	if isFirstTurn {
		_ = s.UpdateSessionTitle(saveCtx, sessionID, req.Input)
	}
	_ = s.TouchSession(saveCtx, sessionID)

	slog.Info("server: turn complete",
		"session", sessionID,
		"latency_ms", inferLatencyMs,
		"reply_bytes", len(reply),
		"client_cancelled", ctx.Err() != nil,
	)

	// message.after hook：fire-and-forget，不阻塞响应
	s.Hooks.Fire("message.after", map[string]string{
		"POLARIS_REPLY":      reply,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    "web",
	})
	// turn.stop hook（对应 ADR-0015 §2.2 Codex Stop 事件语义：Agent 完成本轮回复回到空闲）。
	// 与 message.after 触发点一致但语义独立（Stop 关注"FSM 回到 idle"，message.after 关注
	// "回复已发出"），保留两个事件名以便未来分化，见 00-Global-Dictionary.md §[ShellHooks]。
	s.Hooks.Fire("turn.stop", map[string]string{
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    "web",
	})

	WriteSSE(w, flusher, "complete", map[string]any{
		"session_id":  sessionID,
		"duration_ms": inferLatencyMs,
	})
}

func (s *ChatHandler) handleAgentStreamFSM(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	sessionID string,
	agentCtrl protocol.AgentController,
	input string,
) (string, string, bool) {
	// [W-2-A] 接入 SystemPromptGuard
	systemPromptGuard := guard.NewSystemPromptGuard(0)
	s.ActivatedSystemPromptMu.RLock()
	systemPromptGuard.AddFragment(s.ActivatedSystemPrompt)
	s.ActivatedSystemPromptMu.RUnlock()

	// 先订阅后触发：订阅通道就绪前 FSM 不会开始产出，消除早期事件丢失竞态。
	ch := agentCtrl.SubscribeStream(ctx)

	agentCtrl.SetTaskIntent([]byte(input))
	if err := agentCtrl.SendIntent(types.TriggerIntentReceived); err != nil {
		// FSM 未能推进（intent 队列满/超时）：不进入等待循环，直接把错误交给
		// 上层统一 SSE 错误路径，避免客户端挂到断连。
		slog.Warn("server: fsm advance failed or timeout", "err", err)
		return "", "Agent 状态机未能接收本轮输入，请稍后重试", false
	}

	var reply strings.Builder
	var inferErr string

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return reply.String(), inferErr, false
			}
			switch ev.Type {
			case types.AgentStreamEventThinking:
				WriteSSE(w, flusher, "reasoning", map[string]any{"content": ev.Content})
			case types.AgentStreamEventToken:
				cleaned, err := systemPromptGuard.Scan(ev.Content, true)
				if err != nil {
					slog.Warn("server: system prompt leak detected", "session_id", sessionID, "err", err)
				}
				ev.Content = cleaned
				WriteSSE(w, flusher, "token", map[string]any{"content": ev.Content})
				reply.WriteString(ev.Content)
			case types.AgentStreamEventToolCall:
				msg := fmt.Sprintf("Executing tool %s...", ev.ToolName)
				WriteSSE(w, flusher, "status", map[string]any{"type": "tool_call", "message": msg})
			case types.AgentStreamEventToolResult:
				WriteSSE(w, flusher, "status", map[string]any{"type": "tool_result", "message": ev.Content})
			case types.AgentStreamEventError:
				if inferErr == "" {
					inferErr = ev.Content
				}
				s.WriteSSEError(w, flusher, "fsm_error", ev.Content, sessionID, nil)
			case types.AgentStreamEventStatus:
				if ev.Content == "task_done" {
					return reply.String(), inferErr, false
				}
				WriteSSE(w, flusher, "status", map[string]any{"type": "info", "message": ev.Content})
			}
		case <-ctx.Done():
			// [GD-13-002] 客户端断连时通知 Agent Kernel 强制中止，避免后台无感空跑
			if agentCtrl != nil {
				agentCtrl.Interrupt(types.InterruptRequest{Action: types.InterruptAbort})
			}
			return reply.String(), ctx.Err().Error(), true
		}
	}
}
