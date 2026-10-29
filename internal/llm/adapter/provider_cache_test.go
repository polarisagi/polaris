package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// captureOpenAIBody 用 Mock 传输层捕获 OpenAIAdapter.Infer 实际发出的请求体。
func captureOpenAIBody(t *testing.T, a *OpenAIAdapter, ctx context.Context, opts ...types.InferOption) map[string]any {
	t.Helper()
	var sent map[string]any
	a.client.HTTPClient = &http.Client{Transport: &MockTransport{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &sent)
			body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: http.Header{}}, nil
		},
	}}
	if _, err := a.Infer(ctx, []types.Message{{Role: "user", Content: "hi"}}, opts...); err != nil {
		t.Fatal(err)
	}
	return sent
}

func newTestOpenAI(baseURL string, opts ...OpenAIOption) *OpenAIAdapter {
	pool := llmparent.NewCredentialPool([]string{"k"}, llmparent.StrategyFillFirst)
	return NewOpenAIAdapter(baseURL, "gpt-x", pool, &http.Client{}, nil, opts...)
}

func ctxWithSession(id string) context.Context {
	return context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, id)
}

// ADR-0105 决策三：官方端点带 ctx 会话标识时发送 prompt_cache_key（sha256 前 16 hex，不含原文）。
func TestOpenAIAdapter_PromptCacheKey_Official(t *testing.T) {
	a := newTestOpenAI("", WithOpenAIPromptCacheRetention("24h"))
	if !a.Capabilities().SupportsPromptCacheKey {
		t.Fatal("官方端点应默认支持 prompt_cache_key")
	}
	sent := captureOpenAIBody(t, a, ctxWithSession("session-alice@example.com"))
	key, _ := sent["prompt_cache_key"].(string)
	if len(key) != 16 || strings.ContainsAny(key, "@-gz") || strings.Contains(key, "alice") {
		t.Fatalf("prompt_cache_key 应为 16 位 hex 且不含原文：%q", key)
	}
	if sent["prompt_cache_retention"] != "24h" {
		t.Fatalf("retention 应下发 24h：%v", sent["prompt_cache_retention"])
	}
	// 同会话稳定、不同会话不同。
	again := captureOpenAIBody(t, a, ctxWithSession("session-alice@example.com"))
	other := captureOpenAIBody(t, a, ctxWithSession("session-bob"))
	if again["prompt_cache_key"] != key || other["prompt_cache_key"] == key {
		t.Fatal("key 应按会话稳定且互异")
	}
}

// ctx 无会话标识时既不发 key，也不发 retention。
func TestOpenAIAdapter_PromptCacheKey_NoSessionOmitted(t *testing.T) {
	a := newTestOpenAI("", WithOpenAIPromptCacheRetention("in_memory"))
	sent := captureOpenAIBody(t, a, context.Background())
	if _, ok := sent["prompt_cache_key"]; ok {
		t.Fatal("无会话标识不应发送 prompt_cache_key")
	}
	if _, ok := sent["prompt_cache_retention"]; ok {
		t.Fatal("无 key 时不应单发 retention")
	}
}

// 非官方兼容端点（含 Ollama 用法）默认不发送未知字段；显式声明后才发送。
func TestOpenAIAdapter_PromptCacheKey_CompatEndpointOptIn(t *testing.T) {
	a := newTestOpenAI("http://localhost:11434/v1", WithOpenAIPromptCacheRetention("24h"))
	if a.Capabilities().SupportsPromptCacheKey {
		t.Fatal("非官方端点默认不应声明支持")
	}
	sent := captureOpenAIBody(t, a, ctxWithSession("s1"))
	if _, ok := sent["prompt_cache_key"]; ok {
		t.Fatal("不支持的端点不得收到 prompt_cache_key")
	}
	if _, ok := sent["prompt_cache_retention"]; ok {
		t.Fatal("不支持的端点不得收到 prompt_cache_retention")
	}
	on := newTestOpenAI("https://gw.example.com/v1", WithOpenAIPromptCacheKey(true))
	if _, ok := captureOpenAIBody(t, on, ctxWithSession("s1"))["prompt_cache_key"]; !ok {
		t.Fatal("显式开启后应发送 prompt_cache_key")
	}
}

// 非法 retention 被忽略，不会发出上游会拒绝的值。
func TestOpenAIAdapter_InvalidRetentionIgnored(t *testing.T) {
	a := newTestOpenAI("", WithOpenAIPromptCacheRetention("forever"))
	sent := captureOpenAIBody(t, a, ctxWithSession("s"))
	if _, ok := sent["prompt_cache_retention"]; ok {
		t.Fatal("非法 retention 不应发送")
	}
}

// ToolChoice 只在带 tools 时映射为 tool_choice；无 tools 时忽略。
func TestOpenAIAdapter_ToolChoice(t *testing.T) {
	a := newTestOpenAI("")
	tools := []types.ToolSchema{{Name: "t", Parameters: map[string]any{"type": "object"}}}
	sent := captureOpenAIBody(t, a, context.Background(), types.WithTools(tools), types.WithToolChoice("none"))
	if sent["tool_choice"] != "none" {
		t.Fatalf("应下发 tool_choice=none：%v", sent["tool_choice"])
	}
	sent = captureOpenAIBody(t, a, context.Background(), types.WithToolChoice("none"))
	if _, ok := sent["tool_choice"]; ok {
		t.Fatal("无 tools 时不应下发 tool_choice")
	}
	sent = captureOpenAIBody(t, a, context.Background(), types.WithTools(tools))
	if _, ok := sent["tool_choice"]; ok {
		t.Fatal("未设置 ToolChoice 时不应下发")
	}
}

// OpenAI prompt_tokens_details.cached_tokens 必须落入 Usage.CacheHitTokens。
func TestOpenAIUsage_CachedTokensDetails(t *testing.T) {
	var u OpenAIUsage
	raw := `{"prompt_tokens":2048,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":1920}}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	if got := u.toUsage(); got.InputTokens != 2048 || got.CacheHitTokens != 1920 {
		t.Fatalf("usage = %+v", got)
	}
}

// Google 非流式：usageMetadata.cachedContentTokenCount → Usage.CacheHitTokens。
func TestGoogle_NonStreamCachedContentTokenCount(t *testing.T) {
	a := &GoogleAgentPlatformAdapter{model: "gemini-test"}
	var out geminiInferResponse
	raw := `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
	"usageMetadata":{"promptTokenCount":3000,"candidatesTokenCount":5,"cachedContentTokenCount":2500}}`
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	resp, err := a.buildInferResponseFromGemini(&out, "gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 3000 || resp.Usage.CacheHitTokens != 2500 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// Google 流式：末帧 usageMetadata 的 cachedContentTokenCount 同样写入 StreamEvent.Usage.CacheHitTokens。
func TestGoogle_StreamCachedContentTokenCount(t *testing.T) {
	body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]," +
		"\"usageMetadata\":{\"promptTokenCount\":3000,\"candidatesTokenCount\":5,\"cachedContentTokenCount\":2500}}\n\n"
	ch := make(chan types.StreamEvent, 16)
	go func() {
		defer close(ch)
		parseGoogleStream(context.Background(), strings.NewReader(body), ch, "gemini-test", nil)
	}()
	var hit int
	for ev := range ch {
		if ev.Usage.CacheHitTokens > hit {
			hit = ev.Usage.CacheHitTokens
		}
	}
	if hit != 2500 {
		t.Fatalf("流式 CacheHitTokens = %d，期望 2500", hit)
	}
}
