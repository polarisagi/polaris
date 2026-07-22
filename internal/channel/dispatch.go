package channel

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"

	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SendReply 将 Agent 回复发回各聊天平台。
func (m *Manager) SendReply(ctx context.Context, channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage, text string) error {
	if a, ok := cadapter.Lookup(channelType); ok {
		return a.Send(ctx, m, cfg, msg, text)
	}

	switch channelType {
	case "telegram":
		token, _ := cfg["bot_token"].(string)
		if token == "" {
			slog.Warn("telegram: bot_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		payload, err := json.Marshal(map[string]any{"chat_id": msg.ChatID, "text": text})
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "telegram: marshal payload", err)
		}
		url := "https://api.telegram.org/bot" + token + "/sendMessage"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "telegram: new request", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := m.httpClient.Do(req)
		if err != nil {
			slog.Error("telegram: sendMessage", "err", err)
			return apperr.Wrap(apperr.CodeInternal, "telegram: sendMessage", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, ioErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
			if ioErr != nil {
				slog.Warn("telegram: read non-200 body failed", "status", resp.StatusCode, "err", ioErr)
			} else {
				slog.Warn("telegram: sendMessage non-200", "status", resp.StatusCode, "body", string(b), "err", apperr.New(apperr.CodeInternal, "log event"))
			}
		}

	case "discord":
		token, _ := cfg["bot_token"].(string)
		if token == "" {
			slog.Warn("discord: bot_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.DiscordSendMessage(ctx, m.httpClient, token, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "discord: send message", err)
		}

	case "slack":
		botToken, _ := cfg["bot_token"].(string)
		if botToken == "" {
			slog.Warn("slack: bot_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.SlackSendMessage(ctx, m.httpClient, botToken, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "slack: send message", err)
		}

	case "feishu":
		token, _ := cfg["_feishu_token"].(string)
		domain, _ := cfg["_feishu_domain"].(string)
		if token == "" {
			appID, _ := cfg["app_id"].(string)
			appSecret, _ := cfg["app_secret"].(string)
			switch domain {
			case "", "feishu":
				domain = cadapter.FeishuOpenBase
			case "lark":
				domain = cadapter.LarkOpenBase
			}
			var tokErr error
			token, tokErr = cadapter.FeishuGetTenantToken(ctx, m.httpClient, domain, appID, appSecret)
			if tokErr != nil {
				slog.Error("feishu: get token for reply", "err", tokErr)
				return apperr.Wrap(apperr.CodeInternal, "feishu: get token", tokErr)
			}
		}
		if domain == "" {
			domain = cadapter.FeishuOpenBase
		}
		if err := cadapter.FeishuSendMessage(ctx, m.httpClient, domain, token, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "feishu: send message", err)
		}

	case "line":
		accessToken, _ := cfg["channel_access_token"].(string)
		if accessToken == "" {
			slog.Warn("line: channel_access_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		var err error
		if msg.ReplyToken != "" {
			err = cadapter.LineSendMessage(ctx, m.httpClient, accessToken, msg.ReplyToken, text)
		} else {
			err = cadapter.LinePushMessage(ctx, m.httpClient, accessToken, msg.ChatID, text)
		}
		if err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "line: send message", err)
		}

	case "qqbot":
		token, _ := cfg["_qqbot_token"].(string)
		msgType, _ := cfg["_qqbot_msg_type"].(string)
		if token == "" {
			slog.Warn("qqbot: access token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.QqbotSendMessage(ctx, m.httpClient, token, msgType, msg.ChatID, text, cfg); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "qqbot: send message", err)
		}

	case "whatsapp":
		phoneNumberID, _ := cfg["phone_number_id"].(string)
		accessToken, _ := cfg["access_token"].(string)
		if phoneNumberID == "" || accessToken == "" {
			slog.Warn("whatsapp: phone_number_id or access_token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.WhatsappSendMessage(ctx, m.httpClient, phoneNumberID, accessToken, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "whatsapp: send message", err)
		}

	case "dingtalk":
		if msg.ReplyToken == "" {
			slog.Warn("dingtalk: sessionWebhook missing, cannot reply", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.DingTalkSendMessage(ctx, m.httpClient, msg.ReplyToken, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "dingtalk: send message", err)
		}

	case "wecom":
		if v, ok := m.wecomSends.Load(channelID); ok {
			if ch, ok := v.(chan cadapter.WecomSendMsg); ok {
				select {
				case ch <- cadapter.WecomSendMsg{ChatID: msg.ChatID, Text: text}:
				default:
					slog.Warn("wecom: send channel full", "channel", channelID, "err", apperr.New(apperr.CodeInternal, "log event"))
				}
			}
		}
		return nil

	case "mattermost":
		mmURL, _ := cfg["url"].(string)
		token, _ := cfg["token"].(string)
		if mmURL == "" || token == "" {
			slog.Warn("mattermost: url or token missing", "err", apperr.New(apperr.CodeInternal, "log event"))
			return nil
		}
		if err := cadapter.MattermostSendMessage(ctx, m.httpClient, mmURL, token, msg.ChatID, text); err != nil {
			slog.Error("channels: send reply failed", "type", channelType, "err", err)
			return apperr.Wrap(apperr.CodeInternal, "mattermost: send message", err)
		}

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
