package channel

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
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

	// 1. Webhook channel sends reply
	if err := mgr.SendReply(ctx, "webhook", "ch1", map[string]any{}, msg, text); err != nil {
		t.Fatalf("webhook send failed: %v", err)
	}

	// 2. Unknown channel returns error (GR-10.2-001)
	err := mgr.SendReply(ctx, "unknown", "ch1", map[string]any{}, msg, text)
	if err == nil {
		t.Fatal("expected error for unknown channel type")
	}
	if apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("expected CodeInvalidInput, got %s: %v", apperr.CodeOf(err), err)
	}

	// 3. 移至插件的第三方 IM 平台均返回 CodeInvalidInput
	for _, platform := range []string{"telegram", "discord", "slack", "feishu", "wecom"} {
		err := mgr.SendReply(ctx, platform, "ch1", map[string]any{}, msg, text)
		if err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
			t.Fatalf("migrated platform %s should return CodeInvalidInput, got %v", platform, err)
		}
	}
}
