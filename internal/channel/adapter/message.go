package adapter

import (
	"github.com/polarisagi/polaris/internal/protocol"
)

type MessageHandler func(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage)

type PollerHost = protocol.PollerHost
