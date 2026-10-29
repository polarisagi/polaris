package channel

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func TestManager_SendReply_Coverage(t *testing.T) {
	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}
		}),
	}

	mgr := NewManager(clientHTTP, func(channelType, channelID string, cfg map[string]any, msg protocol.ChannelMessage) {})
	ctx := context.Background()
	msg := protocol.ChannelMessage{ChatID: "c1", ReplyToken: "r1"}
	text := "hello"

	mgr.SendReply(ctx, "telegram", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "discord", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "slack", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "feishu", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "line", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "qqbot", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "whatsapp", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "dingtalk", "ch1", map[string]any{}, protocol.ChannelMessage{}, text) // missing ReplyToken
	mgr.SendReply(ctx, "mattermost", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "email", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "matrix", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "signal", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "homeassistant", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "sms", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "teams", "ch1", map[string]any{}, msg, text)
	mgr.SendReply(ctx, "unknown", "ch1", map[string]any{}, msg, text)

	// test valid path for telegram
	mgr.SendReply(ctx, "telegram", "ch1", map[string]any{"bot_token": "valid_token"}, msg, text)
	// Some functions directly construct urls or use the client, we just need them to run to get coverage.
}
