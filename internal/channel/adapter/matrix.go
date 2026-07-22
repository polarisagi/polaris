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
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func RunMatrixPoller(ctx context.Context, host PollerHost, channelID, homeserver, accessToken string, cfg map[string]any) { //nolint:gocyclo
	slog.Info("matrix: poller started", "channel", channelID)
	defer slog.Info("matrix: poller stopped", "channel", channelID)

	if accessToken == "" {
		username, _ := cfg["username"].(string)
		password, _ := cfg["password"].(string)
		if username != "" && password != "" {
			tok, err := matrixLogin(ctx, host.HTTPClient(), homeserver, username, password)
			if err != nil {
				slog.Error("matrix: login failed", "err", err)
				return
			}
			accessToken = tok
		} else {
			slog.Error("matrix: access_token or username+password required", "err", apperr.New(apperr.CodeInternal, "log event"))
			return
		}
	}

	allowedRooms := make(map[string]bool)
	if ar, _ := cfg["allowed_rooms"].(string); ar != "" {
		for r := range strings.SplitSeq(ar, ",") {
			if r = strings.TrimSpace(r); r != "" {
				allowedRooms[r] = true
			}
		}
	}

	var since string
	backoff := 2 * time.Second

	for {
		nextBatch, events, err := matrixSync(ctx, host.HTTPClient(), homeserver, accessToken, since)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("matrix: sync error", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 2 * time.Second
		since = nextBatch

		for _, ev := range events {
			if len(allowedRooms) > 0 && !allowedRooms[ev.RoomID] {
				continue
			}
			concurrent.SafeGo(ctx, "channel_adapter.matrix.on_message", func(context.Context) {
				host.OnMessage("matrix", channelID, cfg, protocol.ChannelMessage{
					Text: ev.Content.Body, ChatID: ev.RoomID, UserID: ev.Sender,

					TaintLevel: types.TaintHigh,
				})
			})
		}
	}
}

func matrixLogin(ctx context.Context, client *http.Client, homeserver, username, password string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"type":       "m.login.password",
		"identifier": map[string]any{"type": "m.id.user", "user": username},
		"password":   password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/_matrix/client/v3/login", homeserver), bytes.NewReader(body))
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "matrixLogin", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "matrixLogin", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode != http.StatusOK {
		return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("matrix: login status %d: %s", resp.StatusCode, b))
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(b, &result) != nil {
		return "", apperr.New(apperr.CodeInternal, "matrix: decode login response")
	}
	return result.AccessToken, nil
}

type matrixTextEvent struct {
	RoomID  string
	Sender  string
	Content struct {
		MsgType string `json:"msgtype"`
		Body    string `json:"body"`
	}
}

func matrixSync(ctx context.Context, client *http.Client, homeserver, accessToken, since string) (string, []matrixTextEvent, error) {
	url := fmt.Sprintf("%s/_matrix/client/v3/sync?timeout=30000", homeserver)
	if since != "" {
		url += "&since=" + since
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, "matrixSync", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	syncClient := &http.Client{Timeout: 40 * time.Second}
	resp, err := syncClient.Do(req)
	if err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, "matrixSync", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode != http.StatusOK {
		return "", nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("matrix: sync status %d: %s", resp.StatusCode, b))
	}

	var result struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]struct {
				Timeline struct {
					Events []struct {
						Type    string `json:"type"`
						Sender  string `json:"sender"`
						Content struct {
							MsgType string `json:"msgtype"`
							Body    string `json:"body"`
						} `json:"content"`
					} `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if json.Unmarshal(b, &result) != nil {
		return "", nil, apperr.New(apperr.CodeInternal, "matrix: decode sync response")
	}

	var events []matrixTextEvent
	for roomID, room := range result.Rooms.Join {
		for _, ev := range room.Timeline.Events {
			if ev.Type != "m.room.message" || ev.Content.MsgType != "m.text" || ev.Content.Body == "" {
				continue
			}
			if since == "" {
				continue // 首次同步跳过历史消息
			}
			events = append(events, matrixTextEvent{
				RoomID: roomID, Sender: ev.Sender,
				Content: struct {
					MsgType string `json:"msgtype"`
					Body    string `json:"body"`
				}{MsgType: ev.Content.MsgType, Body: ev.Content.Body},
			})
		}
	}
	return result.NextBatch, events, nil
}

func (s *MatrixSender) SendMessage(ctx context.Context, client *http.Client, homeserver, accessToken, roomID, text string) error {
	txnID := fmt.Sprintf("polaris_%d", s.txnCounter.Add(1))
	url := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s", homeserver, roomID, txnID)
	body, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MatrixSender.SendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MatrixSender.SendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("matrix: send status %d: %s", resp.StatusCode, b))
	}
	return nil
}

type MatrixSender struct {
	txnCounter atomic.Int64
}

func init() { Register(&MatrixAdapter{}) }

type MatrixAdapter struct {
	MatrixSender
}

func (a *MatrixAdapter) Type() string { return "matrix" }

func (a *MatrixAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	return protocol.ChannelMessage{} // Uses stream poller
}

func (a *MatrixAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	homeserver, _ := cfg["homeserver"].(string)
	accessToken, _ := cfg["access_token"].(string)
	if homeserver == "" || accessToken == "" {
		slog.Warn("matrix: homeserver or access_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	if err := a.SendMessage(ctx, host.HTTPClient(), homeserver, accessToken, msg.ChatID, text); err != nil {
		slog.Error("channels: send reply failed", "type", "matrix", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "matrix: send message", err)
	}
	return nil
}

func (a *MatrixAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	homeserver, _ := cfg["homeserver"].(string)
	accessToken, _ := cfg["access_token"].(string)
	username, _ := cfg["username"].(string)
	password, _ := cfg["password"].(string)

	if homeserver == "" || (accessToken == "" && (username == "" || password == "")) {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	host.RegisterPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.matrix."+channelID, func(ctx context.Context) {
		RunMatrixPoller(ctx, host, channelID, homeserver, accessToken, cfg)
	})
	return true
}
