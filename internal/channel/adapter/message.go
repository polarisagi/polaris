package adapter

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
)

type MessageHandler func(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage)

type PollerHost interface {
	HTTPClient() *http.Client
	OnMessage(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage)
	RegisterPoller(channelID string, cancel context.CancelFunc)
	SafeDialer() protocol.SafeDialer
}
