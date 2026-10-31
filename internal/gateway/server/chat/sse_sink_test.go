package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestSSEFrameDoesNotSerializeEventErrorObject(t *testing.T) {
	marker := "private-provider-token-sentinel"
	name, payload, ok := sseFrame(session.Event{
		Kind: session.KindError,
		Payload: map[string]any{
			"code":    string(apperr.CodeProviderExhausted),
			"kind":    "fsm_error",
			"message": "模型调用失败，请稍后重试",
		},
		Err: apperr.Wrap(apperr.CodeProviderExhausted, "private detail", apperr.New(apperr.CodeInternal, marker)),
	})
	if !ok || name != "error" {
		t.Fatalf("sseFrame = (%q,%v), want error event", name, ok)
	}
	encoded, err := json.Marshal(payload)
	if err != nil || strings.Contains(string(encoded), marker) {
		t.Fatalf("SSE projection leaked Event.Err: %s (%v)", encoded, err)
	}
}
