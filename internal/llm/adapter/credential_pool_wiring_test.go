package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestAnthropicAdapter_CredentialPool_RotatesAwayFromFailingKey 验证
// 2026-07-12 P1 修复：keyInjectRT 从静态单 Key 闭包改为 CredentialPool 后，
// 一个 Key 收到 401 应自动进入冷却，后续调用改用池中其他可用 Key，而不是
// "单 Key 失效即整个 Provider 不可用"。
func TestAnthropicAdapter_CredentialPool_RotatesAwayFromFailingKey(t *testing.T) {
	okResp := map[string]interface{}{
		"id":    "msg_ok",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-opus-20240229",
		"content": []map[string]interface{}{
			{"type": "text", "text": "good key response"},
		},
		"usage": map[string]interface{}{"input_tokens": 1, "output_tokens": 1},
	}
	okBody, _ := json.Marshal(okResp)

	client := &http.Client{
		Transport: &MockTransport{
			RoundTripFunc: func(req *http.Request) (*http.Response, error) {
				switch req.Header.Get("x-api-key") {
				case "bad-key":
					return &http.Response{
						StatusCode: 401,
						Body:       io.NopCloser(bytes.NewBufferString(`{"error":"invalid api key"}`)),
						Header:     make(http.Header),
					}, nil
				case "good-key":
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(bytes.NewBuffer(okBody)),
						Header:     make(http.Header),
					}, nil
				default:
					t.Fatalf("unexpected api key on request: %q", req.Header.Get("x-api-key"))
					return nil, nil
				}
			},
		},
	}

	// StrategyFillFirst：只要 bad-key 可用就总是优先选它，401 后应冷却并换到 good-key。
	pool := llmparent.NewCredentialPool([]string{"bad-key", "good-key"}, llmparent.StrategyFillFirst)
	adapter := NewAnthropicAdapter("claude-3-opus-20240229", pool, client, nil)

	msgs := []types.Message{{Role: "user", Content: "hi"}}

	_, err := adapter.Infer(context.Background(), msgs)
	if err == nil {
		t.Fatal("expected first call (bad-key) to fail with HTTP 401")
	}

	resp, err := adapter.Infer(context.Background(), msgs)
	if err != nil {
		t.Fatalf("expected second call to succeed after bad-key cools down, got err: %v", err)
	}
	if resp.Content != "good key response" {
		t.Fatalf("expected response from good-key, got %q", resp.Content)
	}
}
