package channelsadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// HandleWebhookReceive 接收来自聊天平台的 webhook 推送。
// 路径: POST /v1/webhooks/{type}/{channelID}
//
// 2026-07-07 复核发现：本 handler 此前从未被 server_routes.go 注册为实际路由
// （无任何 mux.HandleFunc("... /v1/webhooks/...", ...) 调用），导致 Slack/
// Discord/Telegram/LINE/WhatsApp/Teams/通用 HMAC 全部 webhook 集成在生产环境
// 完全不可达——已在 server_routes.go 补上注册。
func (h *ChannelsAdmin) HandleWebhookReceive(w http.ResponseWriter, r *http.Request) {
	channelType := r.PathValue("channelType")
	channelID := r.PathValue("channelID")

	var cfgJSON, secret string
	var enabled int
	row := h.DB.QueryRowContext(r.Context(),
		`SELECT config_json,webhook_secret,enabled FROM channels WHERE id=? AND type=?`, channelID, channelType)
	if err := row.Scan(&cfgJSON, &secret, &enabled); err != nil || enabled == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var cfg map[string]any
	json.Unmarshal([]byte(cfgJSON), &cfg) //nolint:errcheck

	// [P1修复] webhook body 读取缺少大小限制，恶意方可发送超大 payload 耗尽内存。
	// 限制为 4MB：足够容纳所有平台的 webhook 消息，远低于 VFS 上传的 100MB。
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if err := h.verifyWebhookSource(w, r, channelType, cfg, body); err != nil {
		slog.Warn("webhook verification failed", "channel", channelID, "err", err)
		http.Error(w, err.Error(), apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}

	msg := h.ChannelMgr.ExtractMessage(channelType, body, r)
	if msg.Text == "" || msg.ChatID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok")) //nolint:errcheck
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	concurrent.SafeGo(protocol.Detach(r.Context()), "gateway.sysadmin.dispatch_channel_message", func(ctx context.Context) {
		h.dispatchChannelMessage(ctx, channelType, channelID, cfg, msg)
	})
	concurrent.SafeGo(protocol.Detach(r.Context()), "gateway.sysadmin.trigger_webhook_automations", func(ctx context.Context) {
		h.Cron.TriggerWebhookAutomations(ctx, channelID, msg.Text)
	})
}

// dispatchChannelMessage 推理 + 发回平台。被 webhook handler 和各平台 poller 共用。
func (h *ChannelsAdmin) dispatchChannelMessage(ctx context.Context, channelType, channelID string, cfg map[string]any, msg cadapter.Message) { //nolint:gocyclo

	// Telegram allowed_user_ids 白名单过滤
	if channelType == "telegram" && msg.UserID != "" { //nolint:nestif
		if allowed, _ := cfg["allowed_user_ids"].(string); strings.TrimSpace(allowed) != "" {
			permitted := false
			for id := range strings.SplitSeq(allowed, ",") {
				if strings.TrimSpace(id) == msg.UserID {
					permitted = true
					break
				}
			}
			if !permitted {
				slog.Info("telegram: message rejected (not in allowlist)", "user_id", msg.UserID)
				return
			}
		}
	}

	// SMS allowed_numbers 过滤
	if channelType == "sms" && msg.UserID != "" { //nolint:nestif
		if allowed, _ := cfg["allowed_numbers"].(string); strings.TrimSpace(allowed) != "" {
			permitted := false
			for num := range strings.SplitSeq(allowed, ",") {
				if strings.TrimSpace(num) == msg.UserID {
					permitted = true
					break
				}
			}
			if !permitted {
				slog.Info("sms: message rejected (not in allowlist)", "from", msg.UserID)
				return
			}
		}
	}

	p := h.Registry.PickProvider("default")
	if p == nil {
		p = h.Registry.PickProvider("general")
	}
	if p == nil {
		slog.Warn("channel dispatch: no provider available", "channel", channelID, "err", apperr.New(apperr.CodeInternal, "log event"))
		return
	}

	sessionKey := fmt.Sprintf("ch_%s_%s", channelID, msg.ChatID)
	if err := h.Chat.EnsureSession(ctx, sessionKey); err != nil {
		slog.Error("channel dispatch: ensureSession", "err", err)
		return
	}

	if blocked, reason := h.Hooks.FireBefore("message.before", map[string]string{
		"POLARIS_MESSAGE":    msg.Text,
		"POLARIS_SESSION_ID": sessionKey,
		"POLARIS_CHANNEL":    channelType,
		"POLARIS_USER_ID":    msg.UserID,
		"POLARIS_CHAT_ID":    msg.ChatID,
	}); blocked {
		slog.Info("channel dispatch: hook blocked message",
			"channel", channelType, "user", msg.UserID, "reason", reason)
		return
	}

	history, _ := h.Chat.ListMessages(ctx, sessionKey)
	history = append(history, types.Message{Role: "user", Content: msg.Text})
	if err := h.Chat.SaveMessage(ctx, sessionKey, "user", msg.Text, "", "", 0); err != nil {
		slog.Error("channel dispatch: saveMessage user", "err", err)
	}

	toolSchemas := h.BuildToolSchemas()
	var sb strings.Builder
	const maxToolRounds = 10
	startInfer := time.Now()
	for range maxToolRounds {
		ch, err := p.StreamInfer(ctx, history,
			types.WithMaxTokens(2048),
			types.WithTemperature(0.7),
			types.WithTools(toolSchemas),
		)
		if err != nil {
			slog.Error("channel dispatch: StreamInfer", "channel", channelID, "err", err)
			return
		}

		var roundText strings.Builder
		var toolCalls []map[string]json.RawMessage
		for ev := range ch {
			switch ev.Type {
			case types.StreamTextDelta:
				if ev.Content != "" {
					roundText.WriteString(ev.Content)
					sb.WriteString(ev.Content)
				}
			case types.StreamToolCall:
				var call map[string]json.RawMessage
				if json.Unmarshal([]byte(ev.Content), &call) == nil {
					toolCalls = append(toolCalls, call)
				}
			}
		}

		// 无 tool_call → 推理结束
		if len(toolCalls) == 0 || h.ToolExec == nil {
			break
		}

		// 构造 assistant message (含 tool_use parts) + user message (tool_result parts)
		assistantParts := make([]any, 0, 1+len(toolCalls))
		if roundText.Len() > 0 {
			assistantParts = assistantParts[0:0] // Reset to reuse slice
			assistantParts = append(assistantParts, map[string]any{"type": "text", "text": roundText.String()})
		}
		toolResultParts := make([]any, 0, len(toolCalls))
		for _, tc := range toolCalls {
			var toolID, toolName string
			var inputRaw json.RawMessage
			if b, ok := tc["id"]; ok {
				json.Unmarshal(b, &toolID) //nolint:errcheck
			}
			if b, ok := tc["name"]; ok {
				json.Unmarshal(b, &toolName) //nolint:errcheck
			}
			if b, ok := tc["input"]; ok {
				inputRaw = b
			}
			assistantParts = append(assistantParts, map[string]any{
				"type": "tool_use", "id": toolID, "name": toolName, "input": inputRaw,
			})

			result, execErr := h.ToolExec(ctx, toolName, inputRaw)
			var resultText string
			if execErr != nil {
				resultText = "error: " + execErr.Error()
			} else if result != nil {
				resultText = string(result.Output)
			}
			slog.Info("channel dispatch: tool executed", "name", toolName, "ok", execErr == nil)
			toolResultParts = append(toolResultParts, map[string]any{
				"type": "tool_result", "tool_use_id": toolID, "content": resultText,
			})
		}
		history = append(history,
			types.Message{Role: "assistant", Parts: assistantParts},
			types.Message{Role: "user", Parts: toolResultParts},
		)
	}
	inferLatencyMs := time.Since(startInfer).Milliseconds()

	reply := sb.String()
	if reply == "" {
		return
	}
	if err := h.Chat.SaveMessage(ctx, sessionKey, "assistant", reply, "", "", inferLatencyMs); err != nil {
		slog.Error("channel dispatch: saveMessage assistant", "err", err)
	}
	_ = h.Chat.UpdateSessionTitle(ctx, sessionKey, msg.Text)
	_ = h.Chat.TouchSession(ctx, sessionKey)

	h.Hooks.Fire("message.after", map[string]string{
		"POLARIS_REPLY":      reply,
		"POLARIS_SESSION_ID": sessionKey,
		"POLARIS_CHANNEL":    channelType,
		"POLARIS_USER_ID":    msg.UserID,
		"POLARIS_CHAT_ID":    msg.ChatID,
	})
	// turn.stop hook：见 chat/sse.go 同名注释（ADR-0015 §2.2 Codex Stop 事件语义）。
	h.Hooks.Fire("turn.stop", map[string]string{
		"POLARIS_SESSION_ID": sessionKey,
		"POLARIS_CHANNEL":    channelType,
		"POLARIS_USER_ID":    msg.UserID,
		"POLARIS_CHAT_ID":    msg.ChatID,
	})

	h.ChannelMgr.SendReply(ctx, channelType, channelID, cfg, msg, reply)
}
