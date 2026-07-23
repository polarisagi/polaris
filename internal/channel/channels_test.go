package channel

import (
	"encoding/json"
	"net/http"
	"testing"
)

// ── extractTeamsWebhook ───────────────────────────────────────────────────────

func TestExtractTeamsWebhook_Valid(t *testing.T) {
	payload := map[string]any{
		"value": []any{
			map[string]any{
				"resourceData": map[string]any{
					"body":   map[string]any{"content": "teams text"},
					"from":   map[string]any{"user": map[string]any{"id": "u1", "displayName": "Alice"}},
					"chatId": "chat1",
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	msg := extractTeamsWebhook(body)
	if msg.Text != "teams text" {
		t.Errorf("expected 'teams text', got %q", msg.Text)
	}
	if msg.ChatID != "chat1" {
		t.Errorf("expected chatID='chat1', got %q", msg.ChatID)
	}
}

func TestExtractTeamsWebhook_EmptyValue(t *testing.T) {
	msg := extractTeamsWebhook([]byte(`{"value":[]}`))
	if msg.Text != "" {
		t.Error("empty value should return empty Message")
	}
}

// ── extractGenericWebhook ─────────────────────────────────────────────────────

func TestExtractGenericWebhook_Valid(t *testing.T) {
	msg := extractGenericWebhook([]byte(`{"content":"generic text"}`))
	if msg.Text != "generic text" {
		t.Errorf("expected 'generic text', got %q", msg.Text)
	}
	if msg.ChatID != "webhook" {
		t.Errorf("expected chatID='webhook', got %q", msg.ChatID)
	}
}

// ── ExtractMessage dispatcher ─────────────────────────────────────────────────

func TestExtractMessage_Telegram(t *testing.T) {
	body := `{"message":{"text":"tg","chat":{"id":1},"from":{"id":2}}}`
	msg := ExtractMessage("telegram", []byte(body), nil)
	if msg.Text != "tg" {
		t.Errorf("expected 'tg', got %q", msg.Text)
	}
}

func TestExtractMessage_Discord(t *testing.T) {
	body := `{"content":"dc","channel_id":"c1","author":{"id":"u1"}}`
	msg := ExtractMessage("discord", []byte(body), nil)
	if msg.Text != "dc" {
		t.Errorf("expected 'dc', got %q", msg.Text)
	}
}

func TestExtractMessage_Unknown(t *testing.T) {
	msg := ExtractMessage("unknown_platform", []byte(`{}`), nil)
	if msg.Text != "" || msg.ChatID != "" {
		t.Error("unknown platform should return empty Message")
	}
}

// ── jsonNestedInt64 ───────────────────────────────────────────────────────────

func TestJSONNestedInt64_Valid(t *testing.T) {
	m := map[string]any{
		"chat": map[string]any{"id": float64(42)},
	}
	got := jsonNestedInt64(m, "chat", "id")
	if got != "42" {
		t.Errorf("expected '42', got %q", got)
	}
}

func TestJSONNestedInt64_MissingKey(t *testing.T) {
	m := map[string]any{"chat": map[string]any{}}
	if got := jsonNestedInt64(m, "chat", "id"); got != "" {
		t.Errorf("missing id should return empty string, got %q", got)
	}
}

func TestJSONNestedInt64_MissingNested(t *testing.T) {
	m := map[string]any{}
	if got := jsonNestedInt64(m, "chat", "id"); got != "" {
		t.Errorf("missing nested key should return empty string, got %q", got)
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
