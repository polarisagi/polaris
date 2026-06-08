package channels

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ── extractTelegramWebhook ────────────────────────────────────────────────────

func TestExtractTelegramWebhook_Valid(t *testing.T) {
	body := `{
		"message": {
			"text": "hello",
			"chat": {"id": 123456},
			"from": {"id": 789}
		}
	}`
	msg := extractTelegramWebhook([]byte(body))
	if msg.Text != "hello" {
		t.Errorf("expected text='hello', got %q", msg.Text)
	}
	if msg.ChatID != "123456" {
		t.Errorf("expected chatID='123456', got %q", msg.ChatID)
	}
	if msg.UserID != "789" {
		t.Errorf("expected userID='789', got %q", msg.UserID)
	}
}

func TestExtractTelegramWebhook_InvalidJSON(t *testing.T) {
	msg := extractTelegramWebhook([]byte("bad-json"))
	if msg.Text != "" || msg.ChatID != "" {
		t.Error("invalid JSON should return empty Message")
	}
}

func TestExtractTelegramWebhook_NoMessage(t *testing.T) {
	msg := extractTelegramWebhook([]byte(`{"update_id": 1}`))
	if msg.Text != "" {
		t.Error("missing message key should return empty Message")
	}
}

// ── extractDiscordWebhook ─────────────────────────────────────────────────────

func TestExtractDiscordWebhook_Valid(t *testing.T) {
	body := `{"content":"hi there","channel_id":"ch1","author":{"id":"u1","bot":false}}`
	msg := extractDiscordWebhook([]byte(body))
	if msg.Text != "hi there" {
		t.Errorf("expected 'hi there', got %q", msg.Text)
	}
	if msg.ChatID != "ch1" {
		t.Errorf("expected chatID='ch1', got %q", msg.ChatID)
	}
}

func TestExtractDiscordWebhook_BotFiltered(t *testing.T) {
	body := `{"content":"bot msg","channel_id":"ch1","author":{"id":"bot1","bot":true}}`
	msg := extractDiscordWebhook([]byte(body))
	if msg.Text != "" {
		t.Error("bot message should be filtered (empty Message)")
	}
}

// ── extractSlackWebhook ───────────────────────────────────────────────────────

func TestExtractSlackWebhook_Valid(t *testing.T) {
	body := `{"event":{"text":"slack text","channel":"C1","user":"U1"}}`
	msg := extractSlackWebhook([]byte(body))
	if msg.Text != "slack text" {
		t.Errorf("expected 'slack text', got %q", msg.Text)
	}
	if msg.ChatID != "C1" {
		t.Errorf("expected chatID='C1', got %q", msg.ChatID)
	}
}

func TestExtractSlackWebhook_BotIDFiltered(t *testing.T) {
	body := `{"event":{"text":"bot","channel":"C1","user":"U1","bot_id":"BBOT"}}`
	msg := extractSlackWebhook([]byte(body))
	if msg.Text != "" {
		t.Error("slack bot_id should be filtered")
	}
}

func TestExtractSlackWebhook_NoEvent(t *testing.T) {
	body := `{"type":"url_verification","challenge":"xyz"}`
	msg := extractSlackWebhook([]byte(body))
	if msg.Text != "" {
		t.Error("no event key should return empty Message")
	}
}

// ── extractFeishuWebhook ──────────────────────────────────────────────────────

func TestExtractFeishuWebhook_Valid(t *testing.T) {
	content, _ := json.Marshal(map[string]string{"text": "feishu text"})
	payload := map[string]any{
		"event": map[string]any{
			"message": map[string]any{
				"content": string(content),
				"chat_id": "oc_abc",
			},
			"sender": map[string]any{
				"sender_id": map[string]any{
					"open_id": "ou_xyz",
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	msg := extractFeishuWebhook(body)
	if msg.Text != "feishu text" {
		t.Errorf("expected 'feishu text', got %q", msg.Text)
	}
	if msg.ChatID != "oc_abc" {
		t.Errorf("expected chatID='oc_abc', got %q", msg.ChatID)
	}
	if msg.UserID != "ou_xyz" {
		t.Errorf("expected userID='ou_xyz', got %q", msg.UserID)
	}
}

// ── extractLineWebhook ────────────────────────────────────────────────────────

func TestExtractLineWebhook_Valid(t *testing.T) {
	body := `{
		"events": [{
			"type": "message",
			"message": {"type": "text", "text": "line msg"},
			"source": {"userId": "U1"},
			"replyToken": "token123"
		}]
	}`
	msg := extractLineWebhook([]byte(body))
	if msg.Text != "line msg" {
		t.Errorf("expected 'line msg', got %q", msg.Text)
	}
	if msg.ReplyToken != "token123" {
		t.Errorf("expected replyToken='token123', got %q", msg.ReplyToken)
	}
}

func TestExtractLineWebhook_NonMessageEvent(t *testing.T) {
	body := `{"events":[{"type":"follow","source":{"userId":"U1"}}]}`
	msg := extractLineWebhook([]byte(body))
	if msg.Text != "" {
		t.Error("non-message event should return empty Message")
	}
}

func TestExtractLineWebhook_EmptyEvents(t *testing.T) {
	msg := extractLineWebhook([]byte(`{"events":[]}`))
	if msg.Text != "" {
		t.Error("empty events should return empty Message")
	}
}

// ── extractQQBotWebhook ───────────────────────────────────────────────────────

func TestExtractQQBotWebhook_Valid(t *testing.T) {
	body := `{"content":"/hello","channel_id":"ch1","author":{"id":"u1"}}`
	msg := extractQQBotWebhook([]byte(body))
	if msg.Text != "/hello" {
		t.Errorf("expected '/hello', got %q", msg.Text)
	}
	if msg.ChatID != "ch1" {
		t.Errorf("expected chatID='ch1', got %q", msg.ChatID)
	}
}

// ── extractWhatsAppWebhook ────────────────────────────────────────────────────

func TestExtractWhatsAppWebhook_Valid(t *testing.T) {
	payload := map[string]any{
		"entry": []any{
			map[string]any{
				"changes": []any{
					map[string]any{
						"value": map[string]any{
							"messages": []any{
								map[string]any{
									"type": "text",
									"text": map[string]any{"body": "wa message"},
									"from": "+1234",
								},
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	msg := extractWhatsAppWebhook(body)
	if msg.Text != "wa message" {
		t.Errorf("expected 'wa message', got %q", msg.Text)
	}
	if msg.ChatID != "+1234" {
		t.Errorf("expected chatID='+1234', got %q", msg.ChatID)
	}
}

func TestExtractWhatsAppWebhook_NonTextType(t *testing.T) {
	payload := map[string]any{
		"entry": []any{
			map[string]any{
				"changes": []any{
					map[string]any{
						"value": map[string]any{
							"messages": []any{
								map[string]any{"type": "image"},
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	msg := extractWhatsAppWebhook(body)
	if msg.Text != "" {
		t.Error("non-text type should return empty Message")
	}
}

// ── extractTwilioWebhook ──────────────────────────────────────────────────────

func TestExtractTwilioWebhook_Valid(t *testing.T) {
	form := url.Values{"Body": {"hello sms"}, "From": {"+9876"}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	msg := extractTwilioWebhook(req)
	if msg.Text != "hello sms" {
		t.Errorf("expected 'hello sms', got %q", msg.Text)
	}
	if msg.ChatID != "+9876" {
		t.Errorf("expected chatID='+9876', got %q", msg.ChatID)
	}
}

func TestExtractTwilioWebhook_NilRequest(t *testing.T) {
	msg := extractTwilioWebhook(nil)
	if msg.Text != "" {
		t.Error("nil request should return empty Message")
	}
}

func TestExtractTwilioWebhook_MissingFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Body=hello"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	msg := extractTwilioWebhook(req)
	if msg.Text != "" {
		t.Error("missing From should return empty Message")
	}
}

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

// ── jsonStr ───────────────────────────────────────────────────────────────────

func TestJSONStr_Valid(t *testing.T) {
	m := map[string]json.RawMessage{
		"name": json.RawMessage(`"Alice"`),
	}
	if got := jsonStr(m, "name"); got != "Alice" {
		t.Errorf("expected 'Alice', got %q", got)
	}
}

func TestJSONStr_MissingKey(t *testing.T) {
	m := map[string]json.RawMessage{}
	if got := jsonStr(m, "missing"); got != "" {
		t.Errorf("missing key should return empty string, got %q", got)
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
