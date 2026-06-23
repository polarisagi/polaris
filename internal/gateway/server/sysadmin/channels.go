package sysadmin

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"

	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/channel"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ChannelConfig 聊天平台集成配置。config_json 存储厂商特有字段。
type ChannelConfig struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Enabled       bool           `json:"enabled"`
	Config        map[string]any `json:"config"`
	WebhookSecret string         `json:"webhook_secret"`
	WebhookURL    string         `json:"webhook_url"` // 只读，由服务器生成
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
}

func (h *SysAdminHandler) HandleListChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT id,name,type,enabled,config_json,webhook_secret,created_at,updated_at FROM channels ORDER BY created_at`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	list := []*ChannelConfig{}
	for rows.Next() {
		c := &ChannelConfig{}
		var enabled int
		var cfgJSON string
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &enabled, &cfgJSON, &c.WebhookSecret, &c.CreatedAt, &c.UpdatedAt); err != nil {
			continue
		}
		c.Enabled = enabled == 1
		json.Unmarshal([]byte(cfgJSON), &c.Config) //nolint:errcheck
		if c.Config == nil {
			c.Config = map[string]any{}
		}
		c.WebhookURL = webhookURL(c.Type, c.ID)
		c.WebhookSecret = "" // 不下发给前端
		list = append(list, c)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"channels": list}) //nolint:errcheck
}

func (h *SysAdminHandler) HandleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var c ChannelConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if c.ID == "" {
		b := make([]byte, 8)
		rand.Read(b) //nolint:errcheck
		c.ID = "ch_" + hex.EncodeToString(b)
	}
	if c.WebhookSecret == "" {
		b := make([]byte, 16)
		rand.Read(b) //nolint:errcheck
		c.WebhookSecret = hex.EncodeToString(b)
	}
	cfgBytes, _ := json.Marshal(c.Config)
	now := time.Now().UTC().Format(time.RFC3339)

	err := h.ChannelRepo.CreateChannel(r.Context(), repo.ChannelRow{
		ID:            c.ID,
		Name:          c.Name,
		Type:          c.Type,
		Enabled:       c.Enabled,
		ConfigJSON:    string(cfgBytes),
		WebhookSecret: c.WebhookSecret,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if c.Enabled {
		h.ChannelMgr.Start(c.ID, c.Type, c.Config)
	}

	c.CreatedAt, c.UpdatedAt = now, now
	c.WebhookURL = webhookURL(c.Type, c.ID)
	c.WebhookSecret = ""
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (h *SysAdminHandler) HandleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channelID")
	var c ChannelConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfgBytes, _ := json.Marshal(c.Config)
	now := time.Now().UTC().Format(time.RFC3339)

	updated, err := h.ChannelRepo.UpdateChannel(r.Context(), repo.ChannelRow{
		ID:         id,
		Name:       c.Name,
		Type:       c.Type,
		Enabled:    c.Enabled,
		ConfigJSON: string(cfgBytes),
		UpdatedAt:  now,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !updated {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	h.ChannelMgr.Stop(id)
	if c.Enabled {
		h.ChannelMgr.Start(id, c.Type, c.Config)
	}

	c.ID = id
	c.UpdatedAt = now
	c.WebhookURL = webhookURL(c.Type, c.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (h *SysAdminHandler) HandleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channelID")
	h.ChannelMgr.Stop(id)
	err := h.ChannelRepo.DeleteChannel(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// handleWebhookReceive 接收来自聊天平台的 webhook 推送。
// 路径: POST /v1/webhooks/{type}/{channelID}
func (h *SysAdminHandler) HandleWebhookReceive(w http.ResponseWriter, r *http.Request) {
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

	msg := channel.ExtractMessage(channelType, body, r)
	if msg.Text == "" || msg.ChatID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok")) //nolint:errcheck
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go h.dispatchChannelMessage(protocol.Detach(r.Context()), channelType, channelID, cfg, msg)
	go h.triggerWebhookAutomations(protocol.Detach(r.Context()), channelID, msg.Text)
}

func (h *SysAdminHandler) triggerWebhookAutomations(ctx context.Context, channelID, text string) {
	rows, err := h.DB.QueryContext(ctx, `
		SELECT id, name, prompt, trigger_type, cron_schedule, channel_id,
		       working_dir, reasoning_effort, result_action,
		       sandbox_level, cedar_rules_json
		FROM automations
		WHERE enabled=1
		  AND (trigger_type='webhook' OR trigger_type='both')
		  AND channel_id=?
		  AND last_run_status != 'running'`,
		channelID)
	if err != nil {
		slog.Warn("triggerWebhookAutomations: query failed", "err", err)
		return
	}
	defer rows.Close()

	var due []automation
	for rows.Next() {
		var a automation
		if err := rows.Scan(
			&a.ID, &a.Name, &a.Prompt, &a.TriggerType, &a.CronSchedule, &a.ChannelID,
			&a.WorkingDir, &a.ReasoningEffort, &a.ResultAction,
			&a.SandboxLevel, &a.CedarRulesJSON,
		); err != nil {
			continue
		}
		due = append(due, a)
	}
	rows.Close()

	for i := range due {
		a := &due[i]
		// 动态拼接上下文文本，让 agent 可以感知收到的 webhook 内容
		// 由于 prompt 只有执行时固定，这里临时在 prompt 后追加收到的文本内容
		originalPrompt := a.Prompt
		if text != "" {
			a.Prompt = a.Prompt + "\n[Webhook Payload]:\n" + text
		}
		h.executeAutomation(ctx, a, "webhook")
		a.Prompt = originalPrompt // revert just in case (though `a` is a local copy)
	}
}

// dispatchChannelMessage 推理 + 发回平台。被 webhook handler 和各平台 poller 共用。
func (h *SysAdminHandler) dispatchChannelMessage(ctx context.Context, channelType, channelID string, cfg map[string]any, msg cadapter.Message) { //nolint:gocyclo

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

	history, _ := h.Chat.LoadMessages(ctx, sessionKey)
	history = append(history, types.Message{Role: "user", Content: msg.Text})
	if err := h.Chat.SaveMessage(ctx, sessionKey, "user", msg.Text, "", 0); err != nil {
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
	if err := h.Chat.SaveMessage(ctx, sessionKey, "assistant", reply, "", inferLatencyMs); err != nil {
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

	h.ChannelMgr.SendReply(ctx, channelType, channelID, cfg, msg, reply)
}

// webhookURL 生成平台 webhook 接收地址（纯函数，无需 Server 接收者）。
func webhookURL(channelType, channelID string) string {
	return "/v1/webhooks/" + channelType + "/" + channelID
}

// verifyWebhookSource 统一校验 Webhook 来源。如果验证失败或处理了特定的握手请求则返回 false。
func (h *SysAdminHandler) verifyWebhookSource(w http.ResponseWriter, r *http.Request, channelType string, cfg map[string]any, body []byte) error {
	switch channelType {
	case "line":
		return h.verifyLineWebhook(w, r, cfg, body)
	case "whatsapp":
		return h.verifyWhatsAppWebhook(w, r, cfg, body)
	case "teams":
		return h.verifyTeamsWebhook(w, r, cfg, body)
	case "telegram":
		return h.verifyTelegramWebhook(w, r, cfg)
	case "slack":
		return h.verifySlackWebhook(w, r, cfg, body)
	case "discord":
		return h.verifyDiscordWebhook(w, r, cfg, body)
	default:
		if secret, _ := cfg["webhook_secret"].(string); secret != "" {
			// 通用 HMAC-SHA256 验证（X-Hub-Signature-256 header）
			return h.verifyGenericHMAC(w, r, secret, body)
		}
		return apperr.New(apperr.CodeUnauthorized, "generic webhook: missing webhook_secret") // fail-closed
	}
}

func (h *SysAdminHandler) verifyLineWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	channelSecret, _ := cfg["channel_secret"].(string)
	sig := r.Header.Get("X-Line-Signature")
	if !cadapter.LineVerifySignature(channelSecret, string(body), sig) {
		return apperr.New(apperr.CodeUnauthorized, "line webhook: signature mismatch")
	}
	return nil
}

func (h *SysAdminHandler) verifyWhatsAppWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	// GET：处理 hub challenge
	if r.Method == http.MethodGet {
		challenge := r.URL.Query().Get("hub.challenge")
		verifyToken, _ := cfg["verify_token"].(string)
		if verifyToken != "" && r.URL.Query().Get("hub.verify_token") != verifyToken {
			return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: verify_token mismatch")
		}
		w.Write([]byte(challenge)) //nolint:errcheck
		return apperr.New(apperr.CodeOK, "hub challenge handled")
	}

	// POST：验证 X-Hub-Signature-256
	appSecret, _ := cfg["app_secret"].(string)
	if appSecret == "" {
		return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: app_secret not configured")
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: missing signature")
	}
	sig = strings.TrimPrefix(sig, "sha256=")
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return apperr.New(apperr.CodeUnauthorized, "whatsapp webhook: signature mismatch")
	}
	return nil
}

func (h *SysAdminHandler) verifyTeamsWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error { //nolint:gocyclo
	vt := r.URL.Query().Get("validationToken")
	if vt != "" {
		if len(vt) > 256 {
			return apperr.New(apperr.CodeInvalidInput, "invalid validationToken")
		}
		for _, c := range vt {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				return apperr.New(apperr.CodeInvalidInput, "invalid validationToken")
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(vt)) //nolint:errcheck
		return apperr.New(apperr.CodeOK, "validationToken handled")
	}

	expectedState, _ := cfg["client_state"].(string)
	if expectedState == "" {
		return apperr.New(apperr.CodeUnauthorized, "teams webhook: client_state not configured")
	}
	var payload struct {
		Value []struct {
			ClientState string `json:"clientState"`
		} `json:"value"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Value) == 0 {
		return apperr.New(apperr.CodeInvalidInput, "teams webhook: bad payload")
	}
	if payload.Value[0].ClientState != expectedState {
		return apperr.New(apperr.CodeUnauthorized, "teams webhook: clientState mismatch")
	}
	return nil
}

func (h *SysAdminHandler) verifyTelegramWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any) error {
	// Telegram setWebhook 的 secret_token 是可选的：
	// 仅当用户在 cfg 中配置了 webhook_secret_token 时做比对，否则直接放行（向后兼容）。
	// bot_token 用于主动 API 调用，不参与 webhook 签名验证。
	secretToken, _ := cfg["webhook_secret_token"].(string)
	if secretToken == "" {
		return nil
	}
	incomingToken := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	// 用 hmac.Equal 做常量时间比对，防止时序攻击
	if !hmac.Equal([]byte(incomingToken), []byte(secretToken)) {
		return apperr.New(apperr.CodeUnauthorized, "telegram webhook: secret token mismatch")
	}
	return nil
}

func (h *SysAdminHandler) verifySlackWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	secret, ok := cfg["signing_secret"].(string)
	if !ok || secret == "" {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: signing_secret not configured")
	}
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	if timestamp == "" {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: missing timestamp")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > 5*time.Minute {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: invalid or expired timestamp")
	}
	sig := r.Header.Get("X-Slack-Signature")
	if !strings.HasPrefix(sig, "v0=") {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: invalid signature format")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("v0:%s:%s", timestamp, body)))
	expectedSig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return apperr.New(apperr.CodeUnauthorized, "slack webhook: signature mismatch")
	}
	return nil
}

func (h *SysAdminHandler) verifyDiscordWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) error {
	pubKeyStr, ok := cfg["public_key"].(string)
	if !ok || pubKeyStr == "" {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: public_key not configured")
	}
	pubKey, err := hex.DecodeString(pubKeyStr)
	if err != nil || len(pubKey) != ed25519.PublicKeySize {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: invalid public_key")
	}
	sigStr := r.Header.Get("X-Signature-Ed25519")
	ts := r.Header.Get("X-Signature-Timestamp")
	if sigStr == "" || ts == "" {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: missing signature or timestamp")
	}
	sig, err := hex.DecodeString(sigStr)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: invalid signature format")
	}
	msg := []byte(ts)
	msg = append(msg, body...)
	if !ed25519.Verify(pubKey, msg, sig) {
		return apperr.New(apperr.CodeUnauthorized, "discord webhook: signature verification failed")
	}
	return nil
}

func (h *SysAdminHandler) verifyGenericHMAC(w http.ResponseWriter, r *http.Request, secret string, body []byte) error {
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		return apperr.New(apperr.CodeUnauthorized, "generic webhook: missing X-Hub-Signature-256")
	}
	sig = strings.TrimPrefix(sig, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return apperr.New(apperr.CodeUnauthorized, "generic webhook: signature mismatch")
	}
	return nil
}
