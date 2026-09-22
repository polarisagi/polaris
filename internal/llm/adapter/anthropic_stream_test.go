package adapter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestParseAnthropicStream_MalformedBody 是 stream_test.go
// TestSSEParser_MalformedBody 在 Anthropic 适配器上的对应版本：HTTP 200 但
// body 不是合法 SSE 帧时，此前每一行都在 `!ok { continue }` 处被静默跳过，
// scanner.Scan() 正常返回 false（真 EOF，此前连 scanner.Err() 都没检查），
// 函数直接落到末尾返回，ch 空、无任何错误信号。必须产出 StreamError。
func TestParseAnthropicStream_MalformedBody(t *testing.T) {
	a := &AnthropicAdapter{}
	ch := make(chan types.StreamEvent, 8)
	body := strings.NewReader("<html><body>502 Bad Gateway</body></html>\n")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	a.parseAnthropicStream(ctx, "claude-3-5-sonnet-latest", body, ch)
	close(ch)

	var gotErr bool
	for ev := range ch {
		if ev.Type == types.StreamError {
			gotErr = true
		}
		if ev.Type == types.StreamTextDelta && ev.Content != "" {
			t.Fatalf("unexpected content from malformed body: %q", ev.Content)
		}
	}

	if !gotErr {
		t.Fatal("expected StreamError for malformed non-SSE 200 body, got silent empty channel")
	}
}

// TestParseAnthropicStream_NormalStop 确认正常场景不受影响：message_stop 之前
// 已经产出过文本，函数应正常 return，不误报 StreamError。
func TestParseAnthropicStream_NormalStop(t *testing.T) {
	a := &AnthropicAdapter{}
	ch := make(chan types.StreamEvent, 8)
	body := strings.NewReader(
		"data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	a.parseAnthropicStream(ctx, "claude-3-5-sonnet-latest", body, ch)
	close(ch)

	var gotText bool
	for ev := range ch {
		if ev.Type == types.StreamError {
			t.Fatalf("unexpected StreamError on normal stream: %s", ev.Content)
		}
		if ev.Type == types.StreamTextDelta && ev.Content == "hi" {
			gotText = true
		}
	}
	if !gotText {
		t.Fatal("expected text delta \"hi\"")
	}
}
