package adapter

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type Message struct {
	Text       string
	ChatID     string
	UserID     string
	ReplyToken string
	TaintLevel types.TaintLevel
}

type MessageHandler func(channelType, channelID string, cfg map[string]any, msg Message)

type PollerHost interface {
	HTTPClient() *http.Client
	OnMessage(channelType, channelID string, cfg map[string]any, msg Message)
	RegisterPoller(channelID string, cancel context.CancelFunc)
	SafeDialer() protocol.SafeDialer
}
