package adapter

import (
	"bufio"
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
)

func RunSignalPoller(ctx context.Context, host PollerHost, channelID, apiURL, account string, cfg map[string]any) {
	slog.Info("signal: poller started", "channel", channelID)
	defer slog.Info("signal: poller stopped", "channel", channelID)

	backoff := 2 * time.Second
	for {
		if err := signalReceiveSSE(ctx, host, channelID, apiURL, account, cfg); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("signal: SSE stream error", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func signalReceiveSSE(ctx context.Context, host PollerHost, channelID, apiURL, account string, cfg map[string]any) error {
	url := fmt.Sprintf("%s/v1/receive/%s", apiURL, account)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Manager.signalReceiveSSE", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Manager.signalReceiveSSE", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("signal: SSE status %d: %s", resp.StatusCode, b))
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var env signalEnvelope
		if json.Unmarshal([]byte(data), &env) != nil {
			continue
		}
		dm := env.Envelope.DataMessage
		if dm == nil || dm.Message == "" {
			continue
		}
		chatID := env.Envelope.SourceNumber
		if dm.GroupInfo != nil && dm.GroupInfo.GroupID != "" {
			chatID = dm.GroupInfo.GroupID
		}
		concurrent.SafeGo(ctx, "channel_adapter.signal.on_message", func(context.Context) {
			host.OnMessage("signal", channelID, cfg, protocol.ChannelMessage{
				Text: dm.Message, ChatID: chatID, UserID: env.Envelope.SourceNumber,

				TaintLevel: types.TaintHigh,
			})
		})
	}
	return scanner.Err()
}

func SignalSendMessage(ctx context.Context, client *http.Client, apiURL, account, recipient, text string) error {
	body, _ := json.Marshal(map[string]any{
		"message": text, "number": account, "recipients": []string{recipient},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v2/send", apiURL), bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SignalSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SignalSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("signal: send status %d: %s", resp.StatusCode, b))
	}
	return nil
}

type signalEnvelope struct {
	Envelope struct {
		SourceNumber string             `json:"sourceNumber"`
		DataMessage  *signalDataMessage `json:"dataMessage"`
	} `json:"envelope"`
}

type signalDataMessage struct {
	Message   string           `json:"message"`
	GroupInfo *signalGroupInfo `json:"groupInfo"`
}

type signalGroupInfo struct {
	GroupID string `json:"groupId"`
}

type SignalAdapter struct{}

func (a *SignalAdapter) Type() string { return "signal" }

func (a *SignalAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	return protocol.ChannelMessage{} // Uses stream poller
}

func (a *SignalAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	apiURL, _ := cfg["api_url"].(string)
	account, _ := cfg["account"].(string)
	if apiURL == "" || account == "" {
		slog.Warn("signal: api_url or account missing", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	if err := SignalSendMessage(ctx, host.HTTPClient(), apiURL, account, msg.ChatID, text); err != nil {
		slog.Error("channels: send reply failed", "type", "signal", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "signal: send message", err)
	}
	return nil
}

func (a *SignalAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	apiURL, _ := cfg["api_url"].(string)
	account, _ := cfg["account"].(string)
	if apiURL == "" || account == "" {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	host.RegisterPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.signal."+channelID, func(ctx context.Context) {
		RunSignalPoller(ctx, host, channelID, apiURL, account, cfg)
	})
	return true
}
