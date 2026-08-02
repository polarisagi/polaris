package channelsadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
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

	chRow, err := h.ChannelRepo.GetChannel(r.Context(), channelID)
	if err != nil || chRow == nil || chRow.Type != channelType || !chRow.Enabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var cfg map[string]any
	json.Unmarshal([]byte(chRow.ConfigJSON), &cfg) //nolint:errcheck

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
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
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

// DispatchChannelMessage 导出入口，供 channel.Manager 作为 poller 入站处理器接线。
func (h *ChannelsAdmin) DispatchChannelMessage(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) {
	// 复用 goroutine-per-message 模型，与 Webhook 入口保持一致并避免阻塞 poller 长连接
	concurrent.SafeGo(context.Background(), "gateway.sysadmin.dispatch_channel_message", func(ctx context.Context) {
		h.dispatchChannelMessage(ctx, channelType, channelID, cfg, msg)
	})
}

// dispatchChannelMessage 推理 + 发回平台。被 webhook handler 和各平台 poller 共用。
func (h *ChannelsAdmin) dispatchChannelMessage(ctx context.Context, channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) { //nolint:gocyclo

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

	// [A-03 Step5] 原内联 EnsureSession/FireBefore("message.before")/
	// SaveMessage(user)/AcquireHeadless/SaveMessage(assistant)/
	// SampleAndScoreReply/UpdateSessionTitle/TouchSession/Fire("message.after")/
	// Fire("turn.stop") 九步序列收敛至 session.Orchestrator.RunTurn
	// (Headless:true) 统一实现，见 internal/gateway/session/
	// orchestrator_headless.go 顶部注释——本分支此前是 workflow/cron/webhook
	// 三者中唯一完整接了 message.before/message.after/turn.stop/TouchSession
	// 的"参照实现"，收敛后 workflow/cron 分支同步补齐这些能力。
	// POLARIS_USER_ID/POLARIS_CHAT_ID 经 Request.Metadata 透传给 Hook 环境变量
	// （Metadata 不覆盖通用键，见 types.go 字段注释）。
	result, err := h.SessionOrch.RunTurn(ctx, session.Request{
		SessionID: sessionKey,
		Input:     msg.Text,
		Channel:   channelType,
		Headless:  true,
		TitleHint: msg.Text,
		Metadata: map[string]string{
			"POLARIS_USER_ID": msg.UserID,
			"POLARIS_CHAT_ID": msg.ChatID,
		},
	}, session.NewBufferSink())
	if err != nil {
		slog.Error("channel dispatch: session.RunTurn failed", "channel", channelID, "err", err)
		return
	}
	if result.Aborted {
		// 拦截原因（hook 拒绝/会话错误）已在 session 包内部记日志，此处无需重复。
		return
	}

	reply := result.Reply
	if reply == "" {
		return
	}

	if err := h.ChannelMgr.SendReply(ctx, channelType, channelID, cfg, msg, reply); err != nil {
		slog.Warn("channel dispatch: send reply failed", "channel", channelID, "err", err)
	}
}
