package adapter

import (
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
