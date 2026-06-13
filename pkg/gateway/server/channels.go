package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/gateway/channels"
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

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
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

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
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

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO channels(id,name,type,enabled,config_json,webhook_secret,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		c.ID, c.Name, c.Type, boolToInt(c.Enabled), string(cfgBytes), c.WebhookSecret, now, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if c.Enabled {
		s.channelMgr.Start(c.ID, c.Type, c.Config)
	}

	c.CreatedAt, c.UpdatedAt = now, now
	c.WebhookURL = webhookURL(c.Type, c.ID)
	c.WebhookSecret = ""
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channelID")
	var c ChannelConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfgBytes, _ := json.Marshal(c.Config)
	now := time.Now().UTC().Format(time.RFC3339)

	res, err := s.db.ExecContext(r.Context(),
		`UPDATE channels SET name=?,type=?,enabled=?,config_json=?,updated_at=? WHERE id=?`,
		c.Name, c.Type, boolToInt(c.Enabled), string(cfgBytes), now, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s.channelMgr.Stop(id)
	if c.Enabled {
		s.channelMgr.Start(id, c.Type, c.Config)
	}

	c.ID = id
	c.UpdatedAt = now
	c.WebhookURL = webhookURL(c.Type, c.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channelID")
	s.channelMgr.Stop(id)
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// handleWebhookReceive 接收来自聊天平台的 webhook 推送。
// 路径: POST /v1/webhooks/{type}/{channelID}
func (s *Server) handleWebhookReceive(w http.ResponseWriter, r *http.Request) {
	channelType := r.PathValue("channelType")
	channelID := r.PathValue("channelID")

	var cfgJSON, secret string
	var enabled int
	row := s.db.QueryRowContext(r.Context(),
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if !s.verifyWebhookSource(w, r, channelType, cfg, body) {
		return
	}

	msg := channels.ExtractMessage(channelType, body, r)
	if msg.Text == "" || msg.ChatID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok")) //nolint:errcheck
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go s.dispatchChannelMessage(protocol.Detach(r.Context()), channelType, channelID, cfg, msg)
	go s.triggerWebhookAutomations(protocol.Detach(r.Context()), channelID, msg.Text)
}

func (s *Server) triggerWebhookAutomations(ctx context.Context, channelID, text string) {
	rows, err := s.db.QueryContext(ctx, `
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
		s.executeAutomation(ctx, a, "webhook")
		a.Prompt = originalPrompt // revert just in case (though `a` is a local copy)
	}
}

// dispatchChannelMessage 推理 + 发回平台。被 webhook handler 和各平台 poller 共用。
func (s *Server) dispatchChannelMessage(ctx context.Context, channelType, channelID string, cfg map[string]any, msg channels.Message) { //nolint:gocyclo

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

	p := s.registry.PickProvider("default")
	if p == nil {
		p = s.registry.PickProvider("general")
	}
	if p == nil {
		slog.Warn("channel dispatch: no provider available", "channel", channelID, "err", perrors.New(perrors.CodeInternal, "log event"))
		return
	}

	sessionKey := fmt.Sprintf("ch_%s_%s", channelID, msg.ChatID)
	if err := s.ensureSession(ctx, sessionKey); err != nil {
		slog.Error("channel dispatch: ensureSession", "err", err)
		return
	}

	if blocked, reason := s.hooks.FireBefore("message.before", map[string]string{
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

	history, _ := s.loadMessages(ctx, sessionKey)
	history = append(history, protocol.Message{Role: "user", Content: msg.Text})
	if err := s.saveMessage(ctx, sessionKey, "user", msg.Text, "", 0); err != nil {
		slog.Error("channel dispatch: saveMessage user", "err", err)
	}

	toolSchemas := s.buildToolSchemas()
	var sb strings.Builder
	const maxToolRounds = 10
	startInfer := time.Now()
	for range maxToolRounds {
		ch, err := p.StreamInfer(ctx, history,
			protocol.WithMaxTokens(2048),
			protocol.WithTemperature(0.7),
			protocol.WithTools(toolSchemas),
		)
		if err != nil {
			slog.Error("channel dispatch: StreamInfer", "channel", channelID, "err", err)
			return
		}

		var roundText strings.Builder
		var toolCalls []map[string]json.RawMessage
		for ev := range ch {
			switch ev.Type {
			case protocol.StreamTextDelta:
				if ev.Content != "" {
					roundText.WriteString(ev.Content)
					sb.WriteString(ev.Content)
				}
			case protocol.StreamToolCall:
				var call map[string]json.RawMessage
				if json.Unmarshal([]byte(ev.Content), &call) == nil {
					toolCalls = append(toolCalls, call)
				}
			}
		}

		// 无 tool_call → 推理结束
		if len(toolCalls) == 0 || s.toolExec == nil {
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

			result, execErr := s.toolExec(ctx, toolName, inputRaw)
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
			protocol.Message{Role: "assistant", Parts: assistantParts},
			protocol.Message{Role: "user", Parts: toolResultParts},
		)
	}
	inferLatencyMs := time.Since(startInfer).Milliseconds()

	reply := sb.String()
	if reply == "" {
		return
	}
	if err := s.saveMessage(ctx, sessionKey, "assistant", reply, "", inferLatencyMs); err != nil {
		slog.Error("channel dispatch: saveMessage assistant", "err", err)
	}
	_ = s.updateSessionTitle(ctx, sessionKey, msg.Text)
	s.touchSession(ctx, sessionKey)

	s.hooks.Fire("message.after", map[string]string{
		"POLARIS_REPLY":      reply,
		"POLARIS_SESSION_ID": sessionKey,
		"POLARIS_CHANNEL":    channelType,
		"POLARIS_USER_ID":    msg.UserID,
		"POLARIS_CHAT_ID":    msg.ChatID,
	})

	s.channelMgr.SendReply(ctx, channelType, channelID, cfg, msg, reply)
}

// webhookURL 生成平台 webhook 接收地址（纯函数，无需 Server 接收者）。
func webhookURL(channelType, channelID string) string {
	return "/v1/webhooks/" + channelType + "/" + channelID
}

// verifyWebhookSource 统一校验 Webhook 来源。如果验证失败或处理了特定的握手请求则返回 false。
func (s *Server) verifyWebhookSource(w http.ResponseWriter, r *http.Request, channelType string, cfg map[string]any, body []byte) bool {
	switch channelType {
	case "line":
		return s.verifyLineWebhook(w, r, cfg, body)
	case "whatsapp":
		return s.verifyWhatsAppWebhook(w, r, cfg)
	case "teams":
		return s.verifyTeamsWebhook(w, r)
	}
	return true
}

func (s *Server) verifyLineWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any, body []byte) bool {
	channelSecret, _ := cfg["channel_secret"].(string)
	sig := r.Header.Get("X-Line-Signature")
	if channelSecret != "" && !channels.LineVerifySignature(channelSecret, string(body), sig) {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) verifyWhatsAppWebhook(w http.ResponseWriter, r *http.Request, cfg map[string]any) bool {
	if r.Method != http.MethodGet {
		return true
	}
	challenge := r.URL.Query().Get("hub.challenge")
	verifyToken, _ := cfg["verify_token"].(string)
	if verifyToken != "" && r.URL.Query().Get("hub.verify_token") != verifyToken {
		w.WriteHeader(http.StatusForbidden)
		return false
	}
	w.Write([]byte(challenge)) //nolint:errcheck
	return false
}

func (s *Server) verifyTeamsWebhook(w http.ResponseWriter, r *http.Request) bool {
	vt := r.URL.Query().Get("validationToken")
	if vt == "" {
		return true
	}
	if len(vt) > 256 {
		http.Error(w, "invalid validationToken", http.StatusBadRequest)
		return false
	}
	for _, c := range vt {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			http.Error(w, "invalid validationToken", http.StatusBadRequest)
			return false
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(vt)) //nolint:errcheck
	return false
}
