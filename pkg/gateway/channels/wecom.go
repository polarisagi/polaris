package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"github.com/gorilla/websocket"
)

const wecomDefaultWSURL = "wss://openws.work.weixin.qq.com"

type wecomSendMsg struct {
	chatID string
	text   string
}

func (m *Manager) startWeComPoller(channelID, botID, secret string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)

	sendCh := make(chan wecomSendMsg, 32)
	m.wecomSends.Store(channelID, sendCh)

	go func() {
		defer m.wecomSends.Delete(channelID)
		m.runWeComPoller(ctx, channelID, botID, secret, cfg, sendCh)
	}()
}

func (m *Manager) runWeComPoller(ctx context.Context, channelID, botID, secret string, cfg map[string]any, sendCh <-chan wecomSendMsg) {
	slog.Info("wecom: poller started", "channel", channelID)
	defer slog.Info("wecom: poller stopped", "channel", channelID)

	wsURL, _ := cfg["ws_url"].(string)
	if wsURL == "" {
		wsURL = wecomDefaultWSURL
	}

	backoff := 2 * time.Second
	for {
		if err := m.wecomConnect(ctx, channelID, botID, secret, wsURL, cfg, sendCh); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("wecom: connection error", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (m *Manager) wecomConnect(ctx context.Context, channelID, botID, secret, wsURL string, cfg map[string]any, sendCh <-chan wecomSendMsg) error { //nolint:gocyclo
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("wecom: dial: %v", err), err)
	}
	defer conn.Close()

	deviceID := fmt.Sprintf("polaris-%s", channelID[:min(8, len(channelID))])
	authMsg := map[string]any{
		"cmd": "aibot_subscribe",
		"headers": map[string]any{
			"req_id": fmt.Sprintf("subscribe-%d", time.Now().UnixNano()),
		},
		"body": map[string]any{
			"bot_id":    botID,
			"secret":    secret,
			"device_id": deviceID,
		},
	}
	if err := conn.WriteJSON(authMsg); err != nil {
		return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("wecom: auth write: %v", err), err)
	}

	var mu sync.Mutex
	writeJSON := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		return conn.WriteJSON(v)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req, ok := <-sendCh:
				if !ok {
					return
				}
				msg := map[string]any{
					"cmd": "aibot_send_msg",
					"headers": map[string]any{
						"req_id": fmt.Sprintf("send-%d", time.Now().UnixNano()),
					},
					"body": map[string]any{
						"chatid":  req.chatID,
						"msgtype": "text",
						"text":    map[string]string{"content": req.text},
					},
				}
				if err := writeJSON(msg); err != nil {
					slog.Error("wecom: send failed", "err", err)
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("wecom: read: %v", err), err)
		}
		var payload struct {
			Cmd     string         `json:"cmd"`
			Headers map[string]any `json:"headers"`
			Body    map[string]any `json:"body"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			continue
		}
		switch payload.Cmd {
		case "ping":
			_ = writeJSON(map[string]any{"cmd": "pong"})
		case "aibot_msg_callback", "aibot_callback":
			body := payload.Body
			if body == nil {
				continue
			}
			msgType, _ := body["msgtype"].(string)
			if strings.ToLower(msgType) != "text" {
				continue
			}
			textBlock, _ := body["text"].(map[string]any)
			content, _ := textBlock["content"].(string)
			content = strings.TrimSpace(content)
			if content == "" {
				continue
			}
			from, _ := body["from"].(map[string]any)
			userID, _ := from["userid"].(string)
			chatID, _ := body["chatid"].(string)
			if chatID == "" {
				chatID = userID
			}
			go m.onMessage("wecom", channelID, cfg, Message{
				Text: content, ChatID: chatID, UserID: userID,
			})
		}
	}
}
