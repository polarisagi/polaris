package adapter

import (
	"bytes"
	"context"
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

const dingTalkStreamEndpointURL = "https://api.dingtalk.com/v1.0/gateway/connections/open"

func RunDingTalkPoller(ctx context.Context, host PollerHost, channelID, clientID, clientSecret string, cfg map[string]any) {
	slog.Info("dingtalk: stream poller started", "channel", channelID)
	defer slog.Info("dingtalk: stream poller stopped", "channel", channelID)

	backoff := 2 * time.Second
	for {
		if err := dingTalkConnect(ctx, host, channelID, clientID, clientSecret, cfg); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("dingtalk: connection error", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func dingTalkConnect(ctx context.Context, host PollerHost, channelID, clientID, clientSecret string, cfg map[string]any) error {
	wsURL, err := dingTalkGetEndpoint(ctx, host.HTTPClient(), clientID, clientSecret)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("dingtalk: get endpoint: %v", err), err)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("dingtalk: dial: %v", err), err)
	}
	defer conn.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("dingtalk: read: %v", err), err)
		}
		var frame dingTalkFrame
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		msgID, _ := frame.Headers["messageId"].(string)

		switch frame.Type {
		case "SYSTEM":
			topic, _ := frame.Headers["topic"].(string)
			if topic == "ping" {
				_ = conn.WriteJSON(map[string]any{
					"code":    200,
					"headers": map[string]any{"messageId": msgID, "topic": "pong"},
					"message": "OK",
					"data":    nil,
				})
			}
		case "EVENT":
			ack := map[string]any{
				"code":    200,
				"headers": map[string]any{"messageId": msgID},
				"message": "OK",
				"data":    nil,
			}
			_ = conn.WriteJSON(ack)

			var evData dingTalkEventData
			if json.Unmarshal([]byte(frame.Data), &evData) != nil {
				continue
			}
			text := strings.TrimSpace(evData.Text.Content)
			if text == "" {
				continue
			}
			chatID := evData.ConversationID
			if chatID == "" {
				chatID = evData.SenderID
			}
			concurrent.SafeGo(ctx, "channel_adapter.dingtalk.on_message", func(context.Context) {
				host.OnMessage("dingtalk", channelID, cfg, protocol.ChannelMessage{
					Text: text, ChatID: chatID, UserID: evData.SenderID, ReplyToken: evData.SessionWebhook,

					TaintLevel: types.TaintHigh,
				})
			})
		}
	}
}

func dingTalkGetEndpoint(ctx context.Context, client *http.Client, clientID, clientSecret string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"clientId": clientID, "clientSecret": clientSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dingTalkStreamEndpointURL, bytes.NewReader(body))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "dingTalkGetEndpoint", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "dingTalkGetEndpoint", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode != http.StatusOK {
		return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("dingtalk: endpoint status %d: %s", resp.StatusCode, b))
	}
	var result struct {
		Endpoint string `json:"endpoint"`
	}
	if json.Unmarshal(b, &result) != nil || result.Endpoint == "" {
		return "", apperr.New(apperr.CodeInternal, "dingtalk: empty endpoint returned")
	}
	return result.Endpoint, nil
}

func DingTalkSendMessage(ctx context.Context, client *http.Client, sessionWebhook, text string) error {
	body, _ := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sessionWebhook, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DingTalkSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DingTalkSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("dingtalk: send status %d: %s", resp.StatusCode, b))
	}
	return nil
}

type dingTalkFrame struct {
	SpecVersion string         `json:"specVersion"`
	Type        string         `json:"type"`
	Headers     map[string]any `json:"headers"`
	Data        string         `json:"data"`
}

type dingTalkEventData struct {
	ConversationID string `json:"conversationId"`
	SenderID       string `json:"senderId"`
	SessionWebhook string `json:"sessionWebhook"`
	Text           struct {
		Content string `json:"content"`
	} `json:"text"`
}

type DingTalkAdapter struct{}

func (a *DingTalkAdapter) Type() string { return "dingtalk" }

func (a *DingTalkAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	return protocol.ChannelMessage{} // Uses stream poller
}

func (a *DingTalkAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	if msg.ReplyToken == "" {
		slog.Warn("dingtalk: sessionWebhook missing, cannot reply", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	if err := DingTalkSendMessage(ctx, host.HTTPClient(), msg.ReplyToken, text); err != nil {
		slog.Error("channels: send reply failed", "type", "dingtalk", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "dingtalk: send message", err)
	}
	return nil
}

func (a *DingTalkAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	clientID, _ := cfg["client_id"].(string)
	clientSecret, _ := cfg["client_secret"].(string)
	if clientID == "" || clientSecret == "" {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	host.RegisterPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.dingtalk."+channelID, func(ctx context.Context) {
		RunDingTalkPoller(ctx, host, channelID, clientID, clientSecret, cfg)
	})
	return true
}
