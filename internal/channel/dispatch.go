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
	if a, ok := cadapter.Lookup(channelType); ok {
		cfg["_channel_id"] = channelID
		return a.Send(ctx, m, cfg, msg, text)
	}

	switch channelType {

	case "teams":
		tenantID, _ := cfg["tenant_id"].(string)
		clientID, _ := cfg["client_id"].(string)
		clientSecret, _ := cfg["client_secret"].(string)
		if tenantID == "" || clientID == "" || clientSecret == "" {
			slog.Warn("teams: tenant_id/client_id/client_secret missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		accessToken, tokenErr := cadapter.TeamsGetAccessToken(ctx, m.httpClient, tenantID, clientID, clientSecret)
		if tokenErr != nil {
			slog.Error("teams: get access token", "err", tokenErr)
			return apperr.Wrap(apperr.CodeInternal, "teams: get access token", tokenErr)
		}
		if err := cadapter.TeamsSendMessage(ctx, m.httpClient, accessToken, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "teams: send message", err)
		}

	default:
		slog.Warn("channels: SendReply not implemented for channel type", "type", channelType)
	}

	return nil
}
