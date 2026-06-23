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

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/gorilla/websocket"
)

const (
	discordGatewayURL = "wss://gateway.discord.gg?v=10&encoding=json"
	discordAPIBase    = "https://discord.com/api/v10"
	discordIntents    = 1 | 512 | 32768 | 4096
)

const (
	discordOpDispatch       = 0
	discordOpHeartbeat      = 1
	discordOpIdentify       = 2
	discordOpResume         = 6
	discordOpReconnect      = 7
	discordOpInvalidSession = 9
	discordOpHello          = 10
	discordOpHeartbeatAck   = 11
)

type discordPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type discordHello struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

type discordReady struct {
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
}

type discordMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
}

func RunDiscordPoller(ctx context.Context, host PollerHost, channelID, token string, cfg map[string]any) {
	slog.Info("discord: gateway started", "channel", channelID)
	defer slog.Info("discord: gateway stopped", "channel", channelID)

	botID, _ := cfg["bot_id"].(string)
	backoff := 2 * time.Second
	var sessionID, resumeURL string
	var lastSeq int64

	for {
		if ctx.Err() != nil {
			return
		}
		gwURL := discordGatewayURL
		if resumeURL != "" {
			gwURL = resumeURL + "?v=10&encoding=json"
		}
		reconnect, newSession, newResume, newSeq := DiscordConnect(ctx, host, channelID, token, botID, gwURL, sessionID, lastSeq, cfg)
		if ctx.Err() != nil {
			return
		}
		if newSession != "" {
			sessionID = newSession
		}
		if newResume != "" {
			resumeURL = newResume
		}
		if newSeq > lastSeq {
			lastSeq = newSeq
		}
		if !reconnect {
			sessionID, resumeURL, lastSeq = "", "", 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func DiscordConnect( //nolint:gocyclo
	ctx context.Context, host PollerHost,
	channelID, token, botID, gatewayURL, sessionID string,
	lastSeq int64,
	cfg map[string]any,
) (canResume bool, newSessionID, newResumeURL string, finalSeq int64) {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, gatewayURL, nil)
	if err != nil {
		slog.Warn("discord: dial failed", "channel", channelID, "err", err)
		return false, "", "", lastSeq
	}
	defer conn.Close()

	var seq atomic.Int64
	seq.Store(lastSeq)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()

	// 读取 HELLO
	_, data, err := conn.ReadMessage()
	if err != nil {
		return false, "", "", seq.Load()
	}
	var p discordPayload
	if json.Unmarshal(data, &p) != nil || p.Op != discordOpHello {
		return false, "", "", seq.Load()
	}
	var hello discordHello
	json.Unmarshal(p.D, &hello) //nolint:errcheck

	// 心跳 goroutine
	go func() {
		jitter := time.Duration(rand.IntN(hello.HeartbeatInterval)) * time.Millisecond
		select {
		case <-heartbeatCtx.Done():
			return
		case <-time.After(jitter):
		}
		ticker := time.NewTicker(time.Duration(hello.HeartbeatInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			s := seq.Load()
			var d json.RawMessage
			if s > 0 {
				d, _ = json.Marshal(s)
			} else {
				d = json.RawMessage("null")
			}
			if err := conn.WriteJSON(discordPayload{Op: discordOpHeartbeat, D: d}); err != nil {
				return
			}
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	if sessionID == "" {
		identD, _ := json.Marshal(map[string]any{
			"token":   "Bot " + token,
			"intents": discordIntents,
			"properties": map[string]string{
				"os": "linux", "browser": "polaris", "device": "polaris",
			},
		})
		conn.WriteJSON(discordPayload{Op: discordOpIdentify, D: identD}) //nolint:errcheck
	} else {
		resumeD, _ := json.Marshal(map[string]any{
			"token": "Bot " + token, "session_id": sessionID, "seq": lastSeq,
		})
		conn.WriteJSON(discordPayload{Op: discordOpResume, D: resumeD}) //nolint:errcheck
	}

	newSessionID = sessionID
	canResume = true

	for {
		if ctx.Err() != nil {
			return canResume, newSessionID, newResumeURL, seq.Load()
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return canResume, newSessionID, newResumeURL, seq.Load()
		}
		var ev discordPayload
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		if ev.S != nil {
			seq.Store(*ev.S)
		}
		switch ev.Op {
		case discordOpReconnect:
			return true, newSessionID, newResumeURL, seq.Load()
		case discordOpInvalidSession:
			var resumable bool
			json.Unmarshal(ev.D, &resumable) //nolint:errcheck
			time.Sleep(time.Duration(1+rand.IntN(4)) * time.Second)
			if !resumable {
				return false, "", "", 0
			}
			return true, newSessionID, newResumeURL, seq.Load()
		case discordOpDispatch:
			switch ev.T {
			case "READY":
				var ready discordReady
				json.Unmarshal(ev.D, &ready) //nolint:errcheck
				newSessionID = ready.SessionID
				newResumeURL = ready.ResumeGatewayURL
			case "MESSAGE_CREATE":
				var dmsg discordMessage
				if json.Unmarshal(ev.D, &dmsg) != nil || dmsg.Author.Bot || dmsg.Content == "" {
					continue
				}
				if botID != "" && dmsg.Author.ID == botID {
					continue
				}
				go host.OnMessage("discord", channelID, cfg, Message{
					Text: dmsg.Content, ChatID: dmsg.ChannelID, UserID: dmsg.Author.ID,
				})
			}
		}
	}
}

func DiscordSendMessage(ctx context.Context, client *http.Client, token, channelID, text string) error {
	body, _ := json.Marshal(map[string]string{"content": text})
	url := fmt.Sprintf("%s/channels/%s/messages", discordAPIBase, channelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DiscordSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+token)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DiscordSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("discord SendMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}
