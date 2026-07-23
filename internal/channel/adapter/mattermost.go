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

func RunMattermostPoller(ctx context.Context, host PollerHost, channelID, mmURL, token string, cfg map[string]any) {
	slog.Info("mattermost: poller started", "channel", channelID)
	defer slog.Info("mattermost: poller stopped", "channel", channelID)

	botUserID, _ := cfg["bot_user_id"].(string)
	allowedUsers := make(map[string]bool)
	if au, _ := cfg["allowed_users"].(string); au != "" {
		for u := range strings.SplitSeq(au, ",") {
			if u = strings.TrimSpace(u); u != "" {
				allowedUsers[u] = true
			}
		}
	}

	backoff := 2 * time.Second
	for {
		if err := mattermostConnect(ctx, host, channelID, mmURL, token, botUserID, allowedUsers, cfg); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("mattermost: connection error", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func mattermostConnect(ctx context.Context, host PollerHost, channelID, mmURL, token, botUserID string, allowedUsers map[string]bool, cfg map[string]any) error {
	wsURL := strings.Replace(mmURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL += "/api/v4/websocket"

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mattermost: dial: %v", err), err)
	}
	defer conn.Close()

	_ = conn.WriteJSON(map[string]any{
		"seq": 1, "action": "authentication_challenge", "data": map[string]any{"token": token},
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mattermost: read: %v", err), err)
		}
		var event mmEvent
		if json.Unmarshal(raw, &event) != nil || event.Event != "posted" {
			continue
		}
		postJSON, _ := event.Data["post"].(string)
		if postJSON == "" {
			continue
		}
		var post mmPost
		if json.Unmarshal([]byte(postJSON), &post) != nil || post.Message == "" {
			continue
		}
		if botUserID != "" && post.UserID == botUserID {
			continue
		}
		if len(allowedUsers) > 0 && !allowedUsers[post.UserID] {
			continue
		}
		concurrent.SafeGo(ctx, "channel_adapter.mattermost.on_message", func(context.Context) {
			host.OnMessage("mattermost", channelID, cfg, protocol.ChannelMessage{
				Text: post.Message, ChatID: post.ChannelID, UserID: post.UserID,

				TaintLevel: types.TaintHigh,
			})
		})
	}
}

func MattermostSendMessage(ctx context.Context, client *http.Client, mmURL, token, channelID, text string) error {
	body, _ := json.Marshal(map[string]any{"channel_id": channelID, "message": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v4/posts", mmURL), bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MattermostSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MattermostSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("mattermost: post status %d: %s", resp.StatusCode, b))
	}
	return nil
}

type mmEvent struct {
	Event string         `json:"event"`
	Data  map[string]any `json:"data"`
}

type mmPost struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Message   string `json:"message"`
}

type MattermostAdapter struct{}

func (a *MattermostAdapter) Type() string { return "mattermost" }

func (a *MattermostAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	return protocol.ChannelMessage{} // Uses stream poller
}

func (a *MattermostAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	mmURL, _ := cfg["url"].(string)
	token, _ := cfg["token"].(string)
	if mmURL == "" || token == "" {
		slog.Warn("mattermost: url or token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	if err := MattermostSendMessage(ctx, host.HTTPClient(), mmURL, token, msg.ChatID, text); err != nil {
		slog.Error("channels: send reply failed", "type", "mattermost", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "mattermost: send message", err)
	}
	return nil
}

func (a *MattermostAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	mmURL, _ := cfg["url"].(string)
	token, _ := cfg["token"].(string)
	if mmURL == "" || token == "" {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	host.RegisterPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.mattermost."+channelID, func(ctx context.Context) {
		RunMattermostPoller(ctx, host, channelID, mmURL, token, cfg)
	})
	return true
}
