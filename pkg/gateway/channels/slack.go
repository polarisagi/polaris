package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"github.com/gorilla/websocket"
)

const (
	slackConnectionsOpen = "https://slack.com/api/apps.connections.open"
	slackPostMessage     = "https://slack.com/api/chat.postMessage"
)

func (m *Manager) startSlackPoller(channelID, botToken, appToken string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	go m.runSlackPoller(ctx, channelID, botToken, appToken, cfg)
}

func (m *Manager) runSlackPoller(ctx context.Context, channelID, botToken, appToken string, cfg map[string]any) {
	slog.Info("slack: socket mode started", "channel", channelID)
	defer slog.Info("slack: socket mode stopped", "channel", channelID)

	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := m.slackSocketConnect(ctx, channelID, botToken, appToken, cfg); err != nil {
			slog.Warn("slack: socket error", "channel", channelID, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (m *Manager) slackSocketConnect(ctx context.Context, channelID, botToken, appToken string, cfg map[string]any) error { //nolint:gocyclo
	wsURL, err := slackGetSocketURL(ctx, m.httpClient, appToken)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("apps.connections.open: %v", err), err)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("dial: %v", err), err)
	}
	defer conn.Close()

	for {
		if ctx.Err() != nil {
			return nil
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("read: %v", err), err)
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(raw, &envelope) != nil {
			continue
		}
		msgType := jsonStr(envelope, "type")
		envelopeID := jsonStr(envelope, "envelope_id")

		switch msgType {
		case "disconnect":
			return perrors.New(perrors.CodeInternal, fmt.Sprintf("server disconnect: %s", jsonStr(envelope, "reason")))
		case "events_api":
			if envelopeID != "" {
				conn.WriteJSON(map[string]string{"envelope_id": envelopeID}) //nolint:errcheck
			}
			var payload struct {
				Event struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Channel string `json:"channel"`
					User    string `json:"user"`
					BotID   string `json:"bot_id"`
				} `json:"event"`
			}
			if payloadRaw, ok := envelope["payload"]; ok {
				_ = json.Unmarshal(payloadRaw, &payload)
			}
			if payload.Event.BotID != "" || payload.Event.Text == "" || payload.Event.Channel == "" {
				continue
			}
			if payload.Event.Type != "message" && payload.Event.Type != "app_mention" {
				continue
			}
			localCfg := make(map[string]any, len(cfg)+1)
			for k, v := range cfg {
				localCfg[k] = v
			}
			localCfg["bot_token"] = botToken
			go m.onMessage("slack", channelID, localCfg, Message{
				Text: payload.Event.Text, ChatID: payload.Event.Channel, UserID: payload.Event.User,
			})
		case "interactive":
			if envelopeID != "" {
				conn.WriteJSON(map[string]string{"envelope_id": envelopeID}) //nolint:errcheck
			}
		}
	}
}

func slackGetSocketURL(ctx context.Context, client *http.Client, appToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackConnectionsOpen, bytes.NewReader([]byte{}))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
	}
	if json.Unmarshal(body, &result) != nil {
		return "", perrors.New(perrors.CodeInternal, fmt.Sprintf("parse: %s", body))
	}
	if !result.OK {
		return "", perrors.New(perrors.CodeInternal, fmt.Sprintf("slack api: %s", body))
	}
	return result.URL, nil
}

func slackSendMessage(ctx context.Context, client *http.Client, botToken, channel, text string) error {
	body, _ := json.Marshal(map[string]string{"channel": channel, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackPostMessage, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("slack postMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}
