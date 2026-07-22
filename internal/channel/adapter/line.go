package adapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func init() { Register(&LineAdapter{}) }

type LineAdapter struct{}

func (a *LineAdapter) Type() string { return "line" }

func (a *LineAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	events, _ := raw["events"].([]any)
	if len(events) == 0 {
		return protocol.ChannelMessage{}
	}
	ev, _ := events[0].(map[string]any)
	evType, _ := ev["type"].(string)
	if evType != "message" {
		return protocol.ChannelMessage{}
	}
	msgObj, _ := ev["message"].(map[string]any)
	msgType, _ := msgObj["type"].(string)
	if msgType != "text" {
		return protocol.ChannelMessage{}
	}
	text, _ := msgObj["text"].(string)
	src, _ := ev["source"].(map[string]any)
	chatID := ""
	if groupID, ok := src["groupId"].(string); ok && groupID != "" {
		chatID = groupID
	} else if userID, ok := src["userId"].(string); ok {
		chatID = userID
	}
	replyToken, _ := ev["replyToken"].(string)
	userID, _ := src["userId"].(string)
	return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: userID, ReplyToken: replyToken, TaintLevel: types.TaintHigh}
}

func (a *LineAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	accessToken, _ := cfg["channel_access_token"].(string)
	if accessToken == "" {
		slog.Warn("line: channel_access_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	var err error
	if msg.ReplyToken != "" {
		err = LineSendMessage(ctx, host.HTTPClient(), accessToken, msg.ReplyToken, text)
	} else {
		err = LinePushMessage(ctx, host.HTTPClient(), accessToken, msg.ChatID, text)
	}
	if err != nil {
		slog.Error("channels: send reply failed", "type", "line", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "line: send message", err)
	}
	return nil
}

func (a *LineAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	// LINE uses webhook, no poller
	return false
}

func LineSendMessage(ctx context.Context, client *http.Client, accessToken, replyToken, text string) error {
	body, _ := json.Marshal(map[string]any{
		"replyToken": replyToken,
		"messages":   []map[string]string{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/v2/bot/message/reply", bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "LineSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "LineSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("line replyMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}

func LinePushMessage(ctx context.Context, client *http.Client, accessToken, to, text string) error {
	body, _ := json.Marshal(map[string]any{
		"to":       to,
		"messages": []map[string]string{{"type": "text", "text": text}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/v2/bot/message/push", bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "LinePushMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "LinePushMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("line pushMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}

// LineVerifySignature 验证 LINE webhook HMAC-SHA256 签名（base64 encoded）。
func LineVerifySignature(channelSecret, body, signatureHeader string) bool {
	if channelSecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(channelSecret))
	mac.Write([]byte(body))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}
