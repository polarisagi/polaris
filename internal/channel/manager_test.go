package channel

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"

	"context"
	"net"
	"net/http"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// mockSafeDialer implements protocol.SafeDialer for testing
type mockSafeDialer struct{}

var _ protocol.SafeDialer = (*mockSafeDialer)(nil)

func (m *mockSafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, nil
}

func TestManager_Lifecycle(t *testing.T) {
	onMsg := func(channelType, channelID string, cfg map[string]any, msg cadapter.Message) {}
	mgr := NewManager(http.DefaultClient, onMsg, WithSafeDialer(&mockSafeDialer{}))

	if mgr.safeDialer == nil {
		t.Error("expected safeDialer to be set")
	}

	// Test registerPoller
	ctx, cancel := context.WithCancel(context.Background())
	mgr.registerPoller("test-1", cancel)
	if _, ok := mgr.pollers["test-1"]; !ok {
		t.Error("expected poller to be registered")
	}

	// Test Stop
	mgr.Stop("test-1")
	if _, ok := mgr.pollers["test-1"]; ok {
		t.Error("expected poller to be removed")
	}
	select {
	case <-ctx.Done():
		// Success
	default:
		t.Error("expected context to be canceled")
	}

	// Test StopAll
	ctx1, cancel1 := context.WithCancel(context.Background())
	_, cancel2 := context.WithCancel(context.Background())
	mgr.registerPoller("t1", cancel1)
	mgr.registerPoller("t2", cancel2)

	mgr.StopAll()
	if len(mgr.pollers) != 0 {
		t.Error("expected all pollers to be removed")
	}
	select {
	case <-ctx1.Done():
	default:
		t.Error("expected t1 context to be canceled")
	}
}

func TestManager_Start_InvalidConfigs(t *testing.T) {
	onMsg := func(channelType, channelID string, cfg map[string]any, msg cadapter.Message) {}
	mgr := NewManager(http.DefaultClient, onMsg)

	// Test Start with invalid/empty configs, should not panic
	mgr.Start("id1", "telegram", map[string]any{})
	mgr.Start("id2", "discord", map[string]any{})
	mgr.Start("id3", "slack", map[string]any{})
	mgr.Start("id4", "feishu", map[string]any{})
	mgr.Start("id5", "qqbot", map[string]any{})
	mgr.Start("id6", "dingtalk", map[string]any{})
	mgr.Start("id7", "wecom", map[string]any{})
	mgr.Start("id8", "mattermost", map[string]any{})
	mgr.Start("id9", "email", map[string]any{})
	mgr.Start("id10", "matrix", map[string]any{})
	mgr.Start("id11", "signal", map[string]any{})
	mgr.Start("id12", "homeassistant", map[string]any{})
	mgr.Start("id13", "unknown_type", map[string]any{})

	if len(mgr.pollers) != 0 {
		t.Errorf("expected 0 pollers started with invalid configs, got %d", len(mgr.pollers))
	}

	// Test Start with valid configs to hit startXXXPoller methods
	mgr.Start("id1", "telegram", map[string]any{"bot_token": "tk"})
	mgr.Start("id2", "discord", map[string]any{"bot_token": "tk"})
	mgr.Start("id3", "slack", map[string]any{"bot_token": "tk", "app_token": "tk"})
	mgr.Start("id4", "feishu", map[string]any{"app_id": "tk", "app_secret": "tk"})
	mgr.Start("id5", "qqbot", map[string]any{"app_id": "tk", "client_secret": "tk"})
	mgr.Start("id6", "dingtalk", map[string]any{"client_id": "tk", "client_secret": "tk"})
	mgr.Start("id7", "wecom", map[string]any{"bot_id": "tk", "secret": "tk"})
	mgr.Start("id8", "mattermost", map[string]any{"url": "tk", "token": "tk"})
	mgr.Start("id9", "email", map[string]any{"imap_host": "tk", "address": "tk", "password": "tk"})
	mgr.Start("id10", "matrix", map[string]any{"homeserver": "tk", "access_token": "tk"})
	mgr.Start("id11", "signal", map[string]any{"api_url": "tk", "account": "tk"})
	mgr.Start("id12", "homeassistant", map[string]any{"url": "tk", "token": "tk"})
}

func TestManager_LoadFromDB_NilDB(t *testing.T) {
	onMsg := func(channelType, channelID string, cfg map[string]any, msg cadapter.Message) {}
	mgr := NewManager(http.DefaultClient, onMsg)

	// Should not panic
	mgr.LoadFromDB(nil)
}
