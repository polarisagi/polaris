package channel

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"

	"context"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SendReply 将 Agent 回复发回各聊天平台。
func (m *Manager) SendReply(ctx context.Context, channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	if a, ok := cadapter.GetAdapter(channelType); ok {
		cfg["_channel_id"] = channelID
		if err := a.Send(ctx, m, cfg, msg, text); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "channel send failed", err)
		}
		return nil
	}
	slog.Warn("channels: SendReply not implemented for channel type", "type", channelType)
	return nil
}
