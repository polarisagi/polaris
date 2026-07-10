package protocol

import (
	"context"

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
