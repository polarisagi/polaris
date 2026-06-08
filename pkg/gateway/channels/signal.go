package channels

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

	perrors "github.com/polarisagi/polaris/internal/errors"
)

func (m *Manager) startSignalPoller(channelID, apiURL, account string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	go m.runSignalPoller(ctx, channelID, apiURL, account, cfg)
}

func (m *Manager) runSignalPoller(ctx context.Context, channelID, apiURL, account string, cfg map[string]any) {
	slog.Info("signal: poller started", "channel", channelID)
	defer slog.Info("signal: poller stopped", "channel", channelID)

	backoff := 2 * time.Second
	for {
		if err := m.signalReceiveSSE(ctx, channelID, apiURL, account, cfg); err != nil {
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

func (m *Manager) signalReceiveSSE(ctx context.Context, channelID, apiURL, account string, cfg map[string]any) error {
	url := fmt.Sprintf("%s/v1/receive/%s", apiURL, account)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("signal: SSE status %d: %s", resp.StatusCode, b))
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
		go m.onMessage("signal", channelID, cfg, Message{
			Text: dm.Message, ChatID: chatID, UserID: env.Envelope.SourceNumber,
		})
	}
	return scanner.Err()
}

func signalSendMessage(ctx context.Context, client *http.Client, apiURL, account, recipient, text string) error {
	body, _ := json.Marshal(map[string]any{
		"message": text, "number": account, "recipients": []string{recipient},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v2/send", apiURL), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("signal: send status %d: %s", resp.StatusCode, b))
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
