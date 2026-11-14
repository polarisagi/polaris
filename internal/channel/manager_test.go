package channel

import (
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
	onMsg := func(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) {}
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

func TestManager_Start_WebhookAndUnknown(t *testing.T) {
	onMsg := func(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) {}
	mgr := NewManager(http.DefaultClient, onMsg)

	// Webhook is webhook only, StartPoller returns false, no poller added
	mgr.Start("id1", "webhook", map[string]any{})
	mgr.Start("id2", "unknown_type", map[string]any{})
	mgr.Start("id3", "telegram", map[string]any{"bot_token": "tk"})

	if len(mgr.pollers) != 0 {
		t.Errorf("expected 0 pollers, got %d", len(mgr.pollers))
	}
}

func TestManager_RestoreChannelsFromDB_NilDB(t *testing.T) {
	onMsg := func(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) {}
	mgr := NewManager(http.DefaultClient, onMsg)

	// Should not panic
	mgr.RestoreChannelsFromDB(nil)
}
