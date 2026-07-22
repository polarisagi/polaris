package channel

import (
	"context"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/pkg/concurrent"
)


func (m *Manager) startHomeAssistantPoller(channelID, haURL, haToken string, cfg map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	m.registerPoller(channelID, cancel)
	concurrent.SafeGo(ctx, "poller.homeassistant."+channelID, func(ctx context.Context) {
		cadapter.RunHomeAssistantPoller(ctx, m, channelID, haURL, haToken, cfg)
	})
}



