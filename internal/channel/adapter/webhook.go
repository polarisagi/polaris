package adapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func init() { Register(&WebhookAdapter{}) }

type WebhookAdapter struct{}

func (a *WebhookAdapter) Type() string { return "webhook" }

func (a *WebhookAdapter) Extract(body []byte, r *http.Request) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	text, _ := raw["content"].(string)
	return protocol.ChannelMessage{Text: text, ChatID: "webhook", TaintLevel: types.TaintHigh}
}

func (a *WebhookAdapter) Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	return nil
}

func (a *WebhookAdapter) StartPoller(host Host, channelID string, cfg map[string]any) bool {
	return false // Webhook is webhook only
}
