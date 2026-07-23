package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/gorilla/websocket"
)

const (
	FeishuOpenBase = "https://open.feishu.cn"
	LarkOpenBase   = "https://open.larksuite.com"
)

type feishuWSFrame struct {
	BizType   string          `json:"biz_type"`
	ReqID     string          `json:"req_id,omitempty"`
	ServiceID int             `json:"service_id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Headers   map[string]any  `json:"headers,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

func RunFeishuPoller(ctx context.Context, host PollerHost, channelID, appID, appSecret string, cfg map[string]any) {
	slog.Info("feishu: ws long connection started", "channel", channelID)
	defer slog.Info("feishu: ws long connection stopped", "channel", channelID)

	domain, _ := cfg["domain"].(string)
	if domain == "lark" {
		domain = LarkOpenBase
	} else {
		domain = FeishuOpenBase
	}

	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := feishuWSConnect(ctx, host, channelID, appID, appSecret, domain, cfg); err != nil {
			slog.Warn("feishu: ws connect error", "channel", channelID, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func feishuWSConnect(ctx context.Context, host PollerHost, channelID, appID, appSecret, domain string, cfg map[string]any) error { //nolint:gocyclo
	token, err := FeishuGetTenantToken(ctx, host.HTTPClient(), domain, appID, appSecret)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("get tenant token: %v", err), err)
	}
	wsURL, err := feishuGetWSEndpoint(ctx, host.HTTPClient(), domain, appID, token)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("get ws endpoint: %v", err), err)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("dial: %v", err), err)
	}
	defer conn.Close()

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	concurrent.SafeGo(heartbeatCtx, "adapter_heartbeat", func(_ context.Context) {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				conn.WriteJSON(map[string]string{"biz_type": "ping"}) //nolint:errcheck
			}
		}
	})

	for {
		if ctx.Err() != nil {
			return nil
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("read: %v", err), err)
		}
		var frame feishuWSFrame
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		if frame.BizType != "event" {
			continue
		}
		var event struct {
			Header struct {
				EventType string `json:"event_type"`
			} `json:"header"`
			Event struct {
				Message struct {
					MessageType string `json:"message_type"`
					Content     string `json:"content"`
					ChatID      string `json:"chat_id"`
				} `json:"message"`
				Sender struct {
					SenderID struct {
						OpenID string `json:"open_id"`
					} `json:"sender_id"`
				} `json:"sender"`
			} `json:"event"`
		}
		if json.Unmarshal(frame.Body, &event) != nil {
			continue
		}
		if event.Header.EventType != "im.message.receive_v1" || event.Event.Message.MessageType != "text" {
			continue
		}
		var textContent struct {
			Text string `json:"text"`
		}
		json.Unmarshal([]byte(event.Event.Message.Content), &textContent) //nolint:errcheck
		text := strings.TrimSpace(textContent.Text)
		if text == "" {
			continue
		}
		if frame.ReqID != "" {
			conn.WriteJSON(map[string]any{"biz_type": "ack", "req_id": frame.ReqID}) //nolint:errcheck
		}
		localCfg := make(map[string]any, len(cfg)+2)
		for k, v := range cfg {
			localCfg[k] = v
		}
		localCfg["_feishu_token"] = token
		localCfg["_feishu_domain"] = domain
		concurrent.SafeGo(ctx, "channel_adapter.feishu.on_message", func(context.Context) {
			host.OnMessage("feishu", channelID, localCfg, protocol.ChannelMessage{
				Text: text, ChatID: event.Event.Message.ChatID, UserID: event.Event.Sender.SenderID.OpenID,

				TaintLevel: types.TaintHigh,
			})
		})
	}
}

func FeishuGetTenantToken(ctx context.Context, client *http.Client, domain, appID, appSecret string) (string, error) {
	url := domain + "/open-apis/auth/v3/tenant_access_token/internal"
	body, _ := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "FeishuGetTenantToken", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "FeishuGetTenantToken", err)
	}
	defer resp.Body.Close()
	var result struct {
		Code              int    `json:"code"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil || result.TenantAccessToken == "" {
		return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("feishu: empty tenant_access_token (code=%d)", result.Code))
	}
	return result.TenantAccessToken, nil
}

func feishuGetWSEndpoint(ctx context.Context, client *http.Client, domain, appID, token string) (string, error) {
	url := domain + "/open-apis/event/v1/im/ws/endpoint?app_id=" + appID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "feishuGetWSEndpoint", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "feishuGetWSEndpoint", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	var result struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
		Code int `json:"code"`
	}
	if json.Unmarshal(b, &result) != nil || result.Data.URL == "" {
		return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("feishu: empty ws endpoint (code=%d)", result.Code))
	}
	return result.Data.URL, nil
}

func FeishuSendMessage(ctx context.Context, client *http.Client, domain, token, chatID, text string) error {
	url := domain + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	content, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]any{
		"receive_id": chatID,
		"content":    string(content),
		"msg_type":   "text",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "FeishuSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "FeishuSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("feishu SendMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}

// FeishuVerifyWebhookSignature 验证飞书 webhook 签名（webhook 模式）。
func FeishuVerifyWebhookSignature(timestamp, nonce, encryptKey, rawBody, signature string) bool {
	if encryptKey == "" {
		return false
	}
	data := timestamp + nonce + encryptKey + rawBody
	h := sha256.Sum256([]byte(data))
	computed := hex.EncodeToString(h[:])
	return computed == signature
}

type FeishuAdapter struct{}

func (a *FeishuAdapter) Type() string { return "feishu" }

func (a *FeishuAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	if ev, ok := raw["event"].(map[string]any); ok { //nolint:nestif
		if m, ok := ev["message"].(map[string]any); ok {
			if content, ok := m["content"].(string); ok {
				var c map[string]any
				if json.Unmarshal([]byte(content), &c) == nil {
					text, _ := c["text"].(string)
					chatID, _ := m["chat_id"].(string)
					senderMap, _ := ev["sender"].(map[string]any)
					senderID, _ := senderMap["sender_id"].(map[string]any)
					openID, _ := senderID["open_id"].(string)
					return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: openID, TaintLevel: types.TaintHigh}
				}
			}
		}
	}
	return protocol.ChannelMessage{}
}

func (a *FeishuAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	token, _ := cfg["_feishu_token"].(string)
	domain, _ := cfg["_feishu_domain"].(string)
	if token == "" || domain == "" {
		appID, _ := cfg["app_id"].(string)
		appSecret, _ := cfg["app_secret"].(string)
		dom, _ := cfg["domain"].(string)
		if dom == "lark" {
			domain = LarkOpenBase
		} else {
			domain = FeishuOpenBase
		}
		if appID == "" || appSecret == "" {
			slog.Warn("feishu: app_id or app_secret missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		t, err := FeishuGetTenantToken(ctx, host.HTTPClient(), domain, appID, appSecret)
		if err != nil {
			slog.Error("channels: send reply failed (feishu token)", "err", err)
			return apperr.Wrap(apperr.CodeInternal, "feishu: get token", err)
		}
		token = t
	}
	if err := FeishuSendMessage(ctx, host.HTTPClient(), domain, token, msg.ChatID, text); err != nil {
		slog.Error("channels: send reply failed", "type", "feishu", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "feishu: send message", err)
	}
	return nil
}

func (a *FeishuAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	appID, _ := cfg["app_id"].(string)
	appSecret, _ := cfg["app_secret"].(string)
	mode, _ := cfg["connection_mode"].(string)
	if appID == "" || appSecret == "" || mode == "webhook" {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	host.RegisterPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.feishu."+channelID, func(ctx context.Context) {
		RunFeishuPoller(ctx, host, channelID, appID, appSecret, cfg)
	})
	return true
}
