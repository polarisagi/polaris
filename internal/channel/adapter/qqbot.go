package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/concurrent"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/gorilla/websocket"
)

const (
	qqbotTokenURL   = "https://bots.qq.com/app/getAppAccessToken"
	qqbotAPIBase    = "https://api.sgroup.qq.com"
	qqbotGatewayAPI = "https://api.sgroup.qq.com/gateway"
	qqbotIntents    = (1 << 30) | (1 << 12) | (1 << 25)
)

const (
	qqbotOpDispatch       = 0
	qqbotOpHeartbeat      = 1
	qqbotOpIdentify       = 2
	qqbotOpResume         = 6
	qqbotOpReconnect      = 7
	qqbotOpInvalidSession = 9
	qqbotOpHello          = 10
	qqbotOpHeartbeatAck   = 11
)

type qqbotPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

func RunQQBotPoller(ctx context.Context, host PollerHost, channelID, appID, clientSecret string, cfg map[string]any) {
	slog.Info("qqbot: gateway started", "channel", channelID)
	defer slog.Info("qqbot: gateway stopped", "channel", channelID)

	backoff := 2 * time.Second
	var sessionID string
	var lastSeq int64

	for {
		if ctx.Err() != nil {
			return
		}
		accessToken, err := qqbotGetAccessToken(ctx, host.HTTPClient(), appID, clientSecret)
		if err != nil {
			slog.Warn("qqbot: get access token failed", "channel", channelID, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 60*time.Second)
			continue
		}
		gatewayURL, err := qqbotGetGatewayURL(ctx, host.HTTPClient(), accessToken)
		if err != nil {
			slog.Warn("qqbot: get gateway url failed", "channel", channelID, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 60*time.Second)
			continue
		}
		canResume, newSessionID, newSeq := qqbotConnect(ctx, host, channelID, appID, accessToken, gatewayURL, sessionID, lastSeq, cfg)
		if ctx.Err() != nil {
			return
		}
		if newSessionID != "" {
			sessionID = newSessionID
		}
		if newSeq > lastSeq {
			lastSeq = newSeq
		}
		if !canResume {
			sessionID = ""
			lastSeq = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func qqbotConnect( //nolint:gocyclo
	ctx context.Context, host PollerHost,
	channelID, appID, accessToken, gatewayURL, sessionID string,
	lastSeq int64,
	cfg map[string]any,
) (canResume bool, newSessionID string, finalSeq int64) {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, gatewayURL, nil)
	if err != nil {
		slog.Warn("qqbot: dial failed", "channel", channelID, "err", err)
		return false, "", lastSeq
	}
	defer conn.Close()

	var seq atomic.Int64
	seq.Store(lastSeq)
	newSessionID = sessionID
	canResume = true

	heartbeatCtx, heartbeatStop := context.WithCancel(ctx)
	defer heartbeatStop()

	_, helloData, err := conn.ReadMessage()
	if err != nil {
		return false, newSessionID, seq.Load()
	}
	var hello qqbotPayload
	_ = json.Unmarshal(helloData, &hello)
	if hello.Op != qqbotOpHello {
		return false, newSessionID, seq.Load()
	}
	var helloD struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	_ = json.Unmarshal(hello.D, &helloD)

	concurrent.SafeGo(heartbeatCtx, "adapter_heartbeat", func(_ context.Context) {
		jitter := time.Duration(rand.IntN(helloD.HeartbeatInterval)) * time.Millisecond
		select {
		case <-heartbeatCtx.Done():
			return
		case <-time.After(jitter):
		}
		ticker := time.NewTicker(time.Duration(helloD.HeartbeatInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			s := seq.Load()
			var d json.RawMessage
			if s > 0 {
				d, _ = json.Marshal(s)
			} else {
				d = json.RawMessage("null")
			}
			conn.WriteJSON(qqbotPayload{Op: qqbotOpHeartbeat, D: d}) //nolint:errcheck
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
			}
		}
	})

	if sessionID == "" {
		identD, _ := json.Marshal(map[string]any{
			"token":      fmt.Sprintf("QQBot %s", accessToken),
			"intents":    qqbotIntents,
			"shard":      []int{0, 1},
			"properties": map[string]string{"$os": "linux"},
		})
		conn.WriteJSON(qqbotPayload{Op: qqbotOpIdentify, D: identD}) //nolint:errcheck
	} else {
		resumeD, _ := json.Marshal(map[string]any{
			"token": fmt.Sprintf("QQBot %s", accessToken), "session_id": sessionID, "seq": lastSeq,
		})
		conn.WriteJSON(qqbotPayload{Op: qqbotOpResume, D: resumeD}) //nolint:errcheck
	}

	for {
		if ctx.Err() != nil {
			return canResume, newSessionID, seq.Load()
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return canResume, newSessionID, seq.Load()
		}
		var p qqbotPayload
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		if p.S != nil {
			seq.Store(*p.S)
		}
		switch p.Op {
		case qqbotOpReconnect:
			return true, newSessionID, seq.Load()
		case qqbotOpInvalidSession:
			time.Sleep(time.Duration(1+rand.IntN(4)) * time.Second)
			return false, "", 0
		case qqbotOpDispatch:
			switch p.T {
			case "READY":
				var ready struct {
					SessionID string `json:"session_id"`
				}
				_ = json.Unmarshal(p.D, &ready)
				newSessionID = ready.SessionID
			case "AT_MESSAGE_CREATE", "C2C_MESSAGE_CREATE", "GROUP_AT_MESSAGE_CREATE", "DIRECT_MESSAGE_CREATE":
				var msg struct {
					ID        string `json:"id"`
					ChannelID string `json:"channel_id"`
					GroupID   string `json:"group_id"`
					OpenID    string `json:"openid"`
					Content   string `json:"content"`
					Author    struct {
						ID string `json:"id"`
					} `json:"author"`
				}
				if json.Unmarshal(p.D, &msg) != nil || msg.Content == "" {
					continue
				}
				chatID := msg.ChannelID
				if chatID == "" {
					chatID = msg.GroupID
				}
				if chatID == "" {
					chatID = msg.OpenID
				}
				localCfg := make(map[string]any, len(cfg)+3)
				for k, v := range cfg {
					localCfg[k] = v
				}
				localCfg["_qqbot_token"] = "QQBot " + accessToken
				localCfg["_qqbot_msg_id"] = msg.ID
				localCfg["_qqbot_msg_type"] = p.T
				go host.OnMessage("qqbot", channelID, localCfg, Message{
					Text: msg.Content, ChatID: chatID, UserID: msg.Author.ID,

					TaintLevel: types.TaintHigh,
				})
			}
		}
	}
}

func qqbotGetAccessToken(ctx context.Context, client *http.Client, appID, clientSecret string) (string, error) {
	body, _ := json.Marshal(map[string]string{"appId": appID, "clientSecret": clientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qqbotTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "qqbotGetAccessToken", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "qqbotGetAccessToken", err)
	}
	defer resp.Body.Close()
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil || result.AccessToken == "" {
		return "", apperr.New(apperr.CodeInternal, "qqbot: empty access_token")
	}
	return result.AccessToken, nil
}

func qqbotGetGatewayURL(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qqbotGatewayAPI, nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "qqbotGetGatewayURL", err)
	}
	req.Header.Set("Authorization", "QQBot "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "qqbotGetGatewayURL", err)
	}
	defer resp.Body.Close()
	var result struct {
		URL string `json:"url"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil || result.URL == "" {
		return "", apperr.New(apperr.CodeInternal, "qqbot: empty gateway url")
	}
	return result.URL, nil
}

func QqbotSendMessage(ctx context.Context, client *http.Client, token, msgType, chatID, text string, cfg map[string]any) error {
	msgID, _ := cfg["_qqbot_msg_id"].(string)
	var apiURL string
	var body map[string]any
	switch msgType {
	case "GROUP_AT_MESSAGE_CREATE":
		apiURL = fmt.Sprintf("%s/v2/groups/%s/messages", qqbotAPIBase, chatID)
		body = map[string]any{"content": text, "msg_type": 0}
	case "C2C_MESSAGE_CREATE":
		apiURL = fmt.Sprintf("%s/v2/users/%s/messages", qqbotAPIBase, chatID)
		body = map[string]any{"content": text, "msg_type": 0}
	default:
		apiURL = fmt.Sprintf("%s/channels/%s/messages", qqbotAPIBase, chatID)
		body = map[string]any{"content": text}
	}
	if msgID != "" {
		body["msg_id"] = msgID
	}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "QqbotSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "QqbotSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("qqbot SendMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}
