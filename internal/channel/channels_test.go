package channel

import (
	"net/http"
	"testing"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
)

// ── ExtractMessage dispatcher ─────────────────────────────────────────────────

func TestExtractMessage_Webhook(t *testing.T) {
	body := `{"content":"webhook message"}`
	msg := ExtractMessage("webhook", []byte(body), nil)
	if msg.Text != "webhook message" {
		t.Errorf("expected 'webhook message', got %q", msg.Text)
	}
	if msg.ChatID != "webhook" {
		t.Errorf("expected 'webhook', got %q", msg.ChatID)
	}
}

func TestRegistry_RegisteredAdapters(t *testing.T) {
	if _, ok := cadapter.GetAdapter("webhook"); !ok {
		t.Error("expected adapter 'webhook' to be registered, but it was not")
	}
	// 16 个第三方 IM 渠道已迁移为插件，内置注册表不再包含它们
	thirdPartyPlatforms := []string{"telegram", "discord", "slack", "feishu", "wecom", "dingtalk", "qqbot"}
	for _, name := range thirdPartyPlatforms {
		if _, ok := cadapter.GetAdapter(name); ok {
			t.Errorf("expected adapter %q to NOT be registered, but it was", name)
		}
	}
}

func TestRegistry_ReturnsSingletonInstance(t *testing.T) {
	a1, ok1 := cadapter.GetAdapter("webhook")
	a2, ok2 := cadapter.GetAdapter("webhook")
	if !ok1 || !ok2 {
		t.Fatal("adapter 'webhook' should be registered")
	}
	if a1 != a2 {
		t.Errorf("GetAdapter('webhook') returned different instances across calls")
	}
}

func TestExtractMessage_Unknown(t *testing.T) {
	msg := ExtractMessage("unknown_platform", []byte(`{}`), nil)
	if msg.Text != "" || msg.ChatID != "" {
		t.Error("unknown platform should return empty Message")
	}
}

// ── NewManager ────────────────────────────────────────────────────────────────

func TestNewManager_NotNil(t *testing.T) {
	m := NewManager(http.DefaultClient, nil)
	if m == nil {
		t.Fatal("NewManager should return non-nil Manager")
	}
}

func TestNewManager_WithSafeDialer(t *testing.T) {
	m := NewManager(http.DefaultClient, nil, WithSafeDialer(nil))
	if m == nil {
		t.Fatal("NewManager with WithSafeDialer should return non-nil Manager")
	}
}
