package channel

import (
	"context"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

func (m *Manager) startDingTalkPoller(channelID, clientID, clientSecret string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.dingtalk."+channelID, func(ctx context.Context) {
		cadapter.RunDingTalkPoller(ctx, m, channelID, clientID, clientSecret, cfg)
	})
}

func (m *Manager) startDiscordPoller(channelID, token string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.discord."+channelID, func(ctx context.Context) {
		cadapter.RunDiscordPoller(ctx, m, channelID, token, cfg)
	})
}

func (m *Manager) startEmailPoller(channelID string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.email."+channelID, func(ctx context.Context) {
		cadapter.RunEmailPoller(ctx, m, channelID, cfg)
	})
}

func (m *Manager) startFeishuPoller(channelID, appID, appSecret string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.feishu."+channelID, func(ctx context.Context) {
		cadapter.RunFeishuPoller(ctx, m, channelID, appID, appSecret, cfg)
	})
}

func (m *Manager) startHomeAssistantPoller(channelID, haURL, haToken string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.homeassistant."+channelID, func(ctx context.Context) {
		cadapter.RunHomeAssistantPoller(ctx, m, channelID, haURL, haToken, cfg)
	})
}

func (m *Manager) startMatrixPoller(channelID, homeserver, accessToken string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.matrix."+channelID, func(ctx context.Context) {
		cadapter.RunMatrixPoller(ctx, m, channelID, homeserver, accessToken, cfg)
	})
}

func (m *Manager) startMattermostPoller(channelID, mmURL, token string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.mattermost."+channelID, func(ctx context.Context) {
		cadapter.RunMattermostPoller(ctx, m, channelID, mmURL, token, cfg)
	})
}

func (m *Manager) startQQBotPoller(channelID, appID, clientSecret string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.qqbot."+channelID, func(ctx context.Context) {
		cadapter.RunQQBotPoller(ctx, m, channelID, appID, clientSecret, cfg)
	})
}

func (m *Manager) startSignalPoller(channelID, apiURL, account string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.signal."+channelID, func(ctx context.Context) {
		cadapter.RunSignalPoller(ctx, m, channelID, apiURL, account, cfg)
	})
}

func (m *Manager) startSlackPoller(channelID, botToken, appToken string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.slack."+channelID, func(ctx context.Context) {
		cadapter.RunSlackPoller(ctx, m, channelID, botToken, appToken, cfg)
	})
}

func (m *Manager) startTelegramPoller(channelID, token string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	poller := cadapter.NewTelegramPoller()
	concurrent.SafeGo(ctx, "poller.telegram."+channelID, func(ctx context.Context) {
		cadapter.RunTelegramPoller(ctx, m, poller, channelID, token, cfg)
	})
}

func (m *Manager) startWeComPoller(channelID, botID, secret string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)

	sendCh := make(chan cadapter.WecomSendMsg, 32)
	m.wecomSends.Store(channelID, sendCh)

	concurrent.SafeGo(ctx, "poller.wecom."+channelID, func(ctx context.Context) {
		defer m.wecomSends.Delete(channelID)
		cadapter.RunWeComPoller(ctx, m, channelID, botID, secret, cfg, sendCh)
	})
}
