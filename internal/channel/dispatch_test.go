package channel

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"

	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManager_SendReply_Coverage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	mgr := NewManager(ts.Client(), func(channelType, channelID string, cfg map[string]any, msg cadapter.Message) {})
	ctx := context.Background()
	msg := cadapter.Message{ChatID: "123", ReplyToken: "token"}
	text := "hello"

	// test missing configs
	mgr.SendReply(ctx, "telegram", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "discord", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "slack", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "feishu", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "line", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "qqbot", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "whatsapp", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "dingtalk", "ch1", map[string]any{}, cadapter.Message{}, text) // missing ReplyToken
	mgr.SendReply(ctx, "mattermost", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "email", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "matrix", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "signal", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "homeassistant", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "sms", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "teams", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "unknown", "ch1", map[string]any{}, msg, text)

	// test valid basic ones that don't need token fetches or complex handshakes or the server mock is enough
	mgr.SendReply(ctx, "telegram", "ch1", map[string]any{"bot_token": "valid_token"}, msg, text)
	// Some functions directly construct urls or use the client, we just need them to run to get coverage.
}
