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

	case "email":
		smtpHost, _ := cfg["smtp_host"].(string)
		smtpPort, _ := cfg["smtp_port"].(string)
		address, _ := cfg["address"].(string)
		password, _ := cfg["password"].(string)
		if smtpPort == "" {
			smtpPort = "587"
		}
		if smtpHost == "" || address == "" || password == "" {
			slog.Warn("email: smtp config missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.EmailSendMessage(smtpHost, smtpPort, address, password, msg.ChatID, "Re: [Polaris]", text); err != nil {
			slog.Error("email: send failed", "to", msg.ChatID, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "email: send failed", err)
		}
		return nil

	case "matrix":
		homeserver, _ := cfg["homeserver"].(string)
		accessToken, _ := cfg["access_token"].(string)
		if homeserver == "" || accessToken == "" {
			slog.Warn("matrix: homeserver or access_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := (&cadapter.MatrixSender{}).SendMessage(ctx, m.httpClient, homeserver, accessToken, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "matrix: send message", err)
		}

	case "signal":
		apiURL, _ := cfg["api_url"].(string)
		account, _ := cfg["account"].(string)
		if apiURL == "" || account == "" {
			slog.Warn("signal: api_url or account missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.SignalSendMessage(ctx, m.httpClient, apiURL, account, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "signal: send message", err)
		}

	case "homeassistant":
		haURL, _ := cfg["url"].(string)
		haToken, _ := cfg["token"].(string)
		if haURL == "" || haToken == "" {
			slog.Warn("homeassistant: url or token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.HaSendPersistentNotification(ctx, m.httpClient, haURL, haToken, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "homeassistant: send notification", err)
		}

	case "sms":
		accountSID, _ := cfg["account_sid"].(string)
		authToken, _ := cfg["auth_token"].(string)
		fromNumber, _ := cfg["from_number"].(string)
		if accountSID == "" || authToken == "" || fromNumber == "" {
			slog.Warn("sms: twilio config missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.TwilioSendSMS(ctx, m.httpClient, accountSID, authToken, fromNumber, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "sms: send message", err)
		}

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
