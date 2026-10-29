package protocol

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/pkg/types"
)

// ChannelMessage is the protocol layer mirror of cadapter.Message.
type ChannelMessage struct {
	Text       string
	ChatID     string
	UserID     string
	ReplyToken string
	TaintLevel types.TaintLevel
}

// ChannelFacade exposes the minimal method set for gateway to send replies.
type ChannelFacade interface {
	SendReply(ctx context.Context, chType, chID string, cfg map[string]any, msg ChannelMessage, content string) error
}

// @consumer internal/channel/manager.go (Channel Manager)
type PollerHost interface {
	HTTPClient() *http.Client
	OnMessage(channelType, channelID string, cfg map[string]any, msg ChannelMessage)
	RegisterPoller(channelID string, cancel context.CancelFunc)
	SafeDialer() SafeDialer
}

// @consumer internal/channel/manager.go (Channel Manager)
type ChannelHost interface {
	PollerHost
}

// @consumer internal/channel/manager.go (Channel Manager)
type ChannelAdapter interface {
	Type() string
	Extract(body []byte, r *http.Request) ChannelMessage
	Send(ctx context.Context, host ChannelHost, cfg map[string]any, msg ChannelMessage, text string) error
	StartPoller(host ChannelHost, channelID string, cfg map[string]any) (started bool)
}
