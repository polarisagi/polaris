package adapter

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestGetAdapter(t *testing.T) {
	a, ok := GetAdapter("webhook")
	if !ok || a == nil || a.Type() != "webhook" {
		t.Fatalf("expected webhook adapter, got %v, %v", a, ok)
	}

	a2, ok := GetAdapter("webhook")
	if !ok || a != a2 {
		t.Fatalf("GetAdapter should return singleton, got %v vs %v", a, a2)
	}

	for _, name := range []string{"telegram", "discord", "slack", "feishu", "unknown"} {
		if _, ok := GetAdapter(name); ok {
			t.Fatalf("adapter %q should not be registered as built-in", name)
		}
	}
}

func TestDeriveClient(t *testing.T) {
	if _, err := deriveClient(nil, time.Second); err == nil {
		t.Fatal("nil base should fail-closed")
	}

	base := &http.Client{Transport: http.DefaultTransport}
	derived, err := deriveClient(base, 5*time.Second)
	if err != nil {
		t.Fatalf("deriveClient failed: %v", err)
	}
	if derived.Timeout != 5*time.Second {
		t.Fatalf("expected timeout 5s, got %v", derived.Timeout)
	}
	if derived.Transport != base.Transport {
		t.Fatal("derived client must inherit Transport")
	}
}

func TestWebhookAdapter_Methods(t *testing.T) {
	a := &WebhookAdapter{}
	if a.Type() != "webhook" {
		t.Fatalf("expected webhook, got %s", a.Type())
	}
	if a.StartPoller(nil, "id", nil) {
		t.Fatal("webhook should not start poller")
	}
	if err := a.Send(context.Background(), nil, nil, protocol.ChannelMessage{}, "hi"); err != nil {
		t.Fatalf("webhook send returned error: %v", err)
	}

	msg := a.Extract([]byte(`{"content":"hello"}`), nil)
	if msg.Text != "hello" || msg.ChatID != "webhook" {
		t.Fatalf("unexpected extract message: %+v", msg)
	}

	badMsg := a.Extract([]byte(`{bad-json`), nil)
	if badMsg.Text != "" {
		t.Fatalf("invalid json should return empty message: %+v", badMsg)
	}
}
