package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type WhatsappAdapter struct{}

func (a *WhatsappAdapter) Type() string { return "whatsapp" }

func (a *WhatsappAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	entry, _ := raw["entry"].([]any)
	if len(entry) == 0 {
		return protocol.ChannelMessage{}
	}
	e, _ := entry[0].(map[string]any)
	changes, _ := e["changes"].([]any)
	if len(changes) == 0 {
		return protocol.ChannelMessage{}
	}
	ch, _ := changes[0].(map[string]any)
	value, _ := ch["value"].(map[string]any)
	messages, _ := value["messages"].([]any)
	if len(messages) == 0 {
		return protocol.ChannelMessage{}
	}
	m, _ := messages[0].(map[string]any)
	msgType, _ := m["type"].(string)
	if msgType != "text" {
		return protocol.ChannelMessage{}
	}
	textObj, _ := m["text"].(map[string]any)
	text, _ := textObj["body"].(string)
	from, _ := m["from"].(string)
	return protocol.ChannelMessage{Text: text, ChatID: from, UserID: from, TaintLevel: types.TaintHigh}
}

func (a *WhatsappAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	phoneNumberID, _ := cfg["phone_number_id"].(string)
	accessToken, _ := cfg["access_token"].(string)
	if phoneNumberID == "" || accessToken == "" {
		slog.Warn("whatsapp: phone_number_id or access_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}
	if err := WhatsappSendMessage(ctx, host.HTTPClient(), phoneNumberID, accessToken, msg.ChatID, text); err != nil {
		slog.Error("channels: send reply failed", "type", "whatsapp", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "whatsapp: send message", err)
	}
	return nil
}

func (a *WhatsappAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	// WhatsApp uses webhooks only
	return false
}

func WhatsappSendMessage(ctx context.Context, client *http.Client, phoneNumberID, accessToken, to, text string) error {
	url := fmt.Sprintf("https://graph.facebook.com/v18.0/%s/messages", phoneNumberID)
	body, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]string{"body": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WhatsappSendMessage", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WhatsappSendMessage", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("whatsapp SendMessage %d: %s", resp.StatusCode, b))
	}
	return nil
}
