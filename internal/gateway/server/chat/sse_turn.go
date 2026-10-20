package chat

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// 回合与连接解耦（ADR-0116）：回合在 TurnHub 持有的 ctx 下后台运行，HTTP 连接只是
// 订阅者。断开只退订，不取消回合；中止必须显式（abort 端点）或守护进程关停。

// sseHeartbeat 空闲心跳间隔：后台回合可能长时间无事件（长工具调用），
// 心跳防止代理/WebView 因空闲掐断订阅连接。
const sseHeartbeat = 15 * time.Second

// turns 返回回合注册表；测试里直接构造的 ChatHandler 可能未经 NewChatHandler，惰性补建。
func (h *ChatHandler) turns() *session.TurnHub {
	h.turnHubOnce.Do(func() {
		if h.TurnHub == nil {
			h.TurnHub = session.NewTurnHub(session.TurnHubConfig{})
		}
	})
	return h.TurnHub
}

// Shutdown 取消全部后台回合并等待其收尾（守护进程关停时调用）。
func (h *ChatHandler) Shutdown(ctx context.Context) {
	h.turns().Shutdown(ctx)
}

// startTurn 在后台启动回合并返回其 Turn；busy/closed 时已向客户端写出错误响应，返回 nil。
func (h *ChatHandler) startTurn(w http.ResponseWriter, r *http.Request, sessionID string, req session.Request) *session.Turn {
	// WithoutCancel 保留请求 ctx 上的认证/追踪 value，但切断连接取消。
	turnCtx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	turn, res := h.turns().Begin(sessionID, cancel)
	switch res {
	case session.BeginBusy:
		cancel()
		httputil.WriteJSONStatus(w, http.StatusConflict, map[string]string{
			"code":    "session_busy",
			"message": "该会话已有正在运行的回合，请等待完成或先中止",
		})
		return nil
	case session.BeginClosed:
		cancel()
		httputil.RespondError(w, "", apperr.New(apperr.CodeInternal, "server shutting down"), http.StatusServiceUnavailable)
		return nil
	}
	concurrent.SafeGo(turnCtx, "gateway.turn", func(ctx context.Context) {
		defer cancel()
		defer turn.Finish()
		if _, err := h.Orchestrator.RunTurn(ctx, req, turn); err != nil {
			// RunTurn 只在会话确保之前的边界校验失败时返回非 nil error；
			// 正常业务失败均已经 sink 推送 KindError 并返回 nil。
			_ = turn.Emit(session.Event{
				Kind:    session.KindError,
				Payload: map[string]any{"code": "turn_error", "message": err.Error()},
			})
		}
	})
	return turn
}

// streamSubscription 把订阅回放/实时事件写成 SSE，直到回合结束、客户端断开。
// 返回即退订；绝不取消回合。
func streamSubscription(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, sub *session.Subscription) {
	defer sub.Close()
	tick := time.NewTicker(sseHeartbeat)
	defer tick.Stop()
	for {
		ev, ok, done := sub.Poll()
		if ok {
			if name, payload, mapped := sseFrame(ev.Event); mapped {
				writeSSEWithID(w, flusher, ev.Seq, name, payload)
			}
			continue
		}
		if done {
			return
		}
		select {
		case <-sub.Wake():
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return // 客户端已断开：仅退订，回合继续（ADR-0116）
			}
			flusher.Flush()
		}
	}
}

func prepareSSE(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关闭 nginx 缓冲
	// SSE 长连接：禁用写超时
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("chat: sse set write deadline unsupported", "error", err)
	}
	return flusher, true
}

// GET /v1/sessions/{sessionID}/stream?after=<seq>
// 重新订阅会话回合：先回放 seq>after 的缓冲事件再接实时；无回合 → 204。
func (h *ChatHandler) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionID"))
	if !session.SessionIDPattern.MatchString(sessionID) {
		httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "invalid session_id"), http.StatusBadRequest)
		return
	}
	var after int64
	raw := r.URL.Query().Get("after")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "invalid after"), http.StatusBadRequest)
			return
		}
		after = n
	}
	turn := h.turns().Get(sessionID)
	if turn == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	flusher, ok := prepareSSE(w)
	if !ok {
		return
	}
	streamSubscription(r.Context(), w, flusher, turn.Subscribe(after))
}

// POST /v1/sessions/{sessionID}/abort 显式中止会话的运行中回合。
func (h *ChatHandler) HandleAbortSession(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionID"))
	if !session.SessionIDPattern.MatchString(sessionID) {
		httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "invalid session_id"), http.StatusBadRequest)
		return
	}
	httputil.WriteJSON(w, map[string]any{"session_id": sessionID, "aborted": h.turns().Abort(sessionID)})
}
