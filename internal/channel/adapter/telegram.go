package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
}

type telegramPoller struct {
	httpClient *http.Client
}

func NewTelegramPoller() *telegramPoller {
	return &telegramPoller{
		httpClient: &http.Client{Timeout: 35 * time.Second},
	}
}

func RunTelegramPoller(ctx context.Context, host PollerHost, poller *telegramPoller, channelID, token string, cfg map[string]any) {
	slog.Info("telegram: long-poll started", "channel", channelID)
	defer slog.Info("telegram: long-poll stopped", "channel", channelID)

	tgDeleteWebhook(ctx, poller.httpClient, token)

	var offset int64
	backoff := 2 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := tgGetUpdates(ctx, poller.httpClient, token, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("telegram: getUpdates error", "channel", channelID, "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = 2 * time.Second

		for _, upd := range updates {
			offset = max(offset, upd.UpdateID+1)
			if upd.Message == nil || upd.Message.Text == "" {
				continue
			}
			msg := protocol.ChannelMessage{
				Text:   upd.Message.Text,
				ChatID: fmt.Sprintf("%d", upd.Message.Chat.ID),
				UserID: fmt.Sprintf("%d", upd.Message.From.ID),

				TaintLevel: types.TaintHigh,
			}
			concurrent.SafeGo(ctx, "channel_adapter.telegram.on_message", func(context.Context) {
				host.OnMessage("telegram", channelID, cfg, msg)
			})
		}
	}
}

func tgGetUpdates(ctx context.Context, client *http.Client, token string, offset int64) ([]tgUpdate, error) {
	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?timeout=30&offset=%d&allowed_updates=[\"message\"]",
		token, offset,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "tgGetUpdates", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "tgGetUpdates", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	var result struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("decode: %v", err), err)
	}
	if !result.OK {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("telegram api: %s", body))
	}
	return result.Result, nil
}

func tgDeleteWebhook(ctx context.Context, client *http.Client, token string) {
	url := "https://api.telegram.org/bot" + token + "/deleteWebhook"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("telegram: deleteWebhook", "err", err)
		return
	}
	resp.Body.Close()
}

type TelegramAdapter struct{}

func (a *TelegramAdapter) Type() string { return "telegram" }

func (a *TelegramAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return protocol.ChannelMessage{}
	}
	text, _ := msg["text"].(string)

	var chatID, userID string
	if chat, ok := msg["chat"].(map[string]any); ok {
		if id, ok := chat["id"].(float64); ok {
			chatID = fmt.Sprintf("%d", int64(id))
		}
	}
	if from, ok := msg["from"].(map[string]any); ok {
		if id, ok := from["id"].(float64); ok {
			userID = fmt.Sprintf("%d", int64(id))
		}
	}
	return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: userID, TaintLevel: types.TaintHigh}
}

func (a *TelegramAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	token, _ := cfg["bot_token"].(string)
	if token == "" {
		slog.Warn("telegram: bot_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	payload, err := json.Marshal(map[string]any{"chat_id": msg.ChatID, "text": text})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "telegram: marshal payload", err)
	}
	url := "https://api.telegram.org/bot" + token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "telegram: new request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := host.HTTPClient().Do(req)
	if err != nil {
		slog.Error("telegram: sendMessage", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "telegram: sendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, ioErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		if ioErr != nil {
			slog.Warn("telegram: read non-200 body failed", "status", resp.StatusCode, "err", ioErr)
		} else {
			slog.Warn("telegram: sendMessage non-200", "status", resp.StatusCode, "body", string(b), "err", apperr.New(apperr.CodeInternal, "log event"))
		}
	}
	return nil
}

func (a *TelegramAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	token, _ := cfg["bot_token"].(string)
	if token == "" {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	host.RegisterPoller(channelID, cancel)
	poller := NewTelegramPoller()
	concurrent.SafeGo(ctx, "poller.telegram."+channelID, func(ctx context.Context) {
		RunTelegramPoller(ctx, host, poller, channelID, token, cfg)
	})
	return true
}
