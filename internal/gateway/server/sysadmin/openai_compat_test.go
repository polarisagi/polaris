package sysadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockStreamProvider struct {
	protocol.Provider
	chunks []types.StreamEvent
	err    error
	hold   bool
}

func (m *mockStreamProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.hold {
		return make(chan types.StreamEvent), nil
	}
	ch := make(chan types.StreamEvent, len(m.chunks))
	for _, c := range m.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}
func (m *mockStreamProvider) ID() string   { return "mock" }
func (m *mockStreamProvider) Name() string { return "mock" }
func (m *mockStreamProvider) Close() error { return nil }
func (m *mockStreamProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *mockStreamProvider) MaxConcurrency() int             { return 1 }
func (m *mockStreamProvider) SupportsModel(model string) bool { return true }
func (m *mockStreamProvider) ModelID() string                 { return "test-model" }

func TestOpenAIChat_Sync(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	registry.RegisterWithRole("mock", "mock", "default", &mockStreamProvider{
		chunks: []types.StreamEvent{
			{Type: types.StreamTextDelta, Content: "Hello"},
			{Type: types.StreamTextDelta, Content: " world"},
		},
	})

	router := llm.NewInferenceRouter(registry, nil)
	h := &SysAdminHandler{Registry: registry, Router: router}

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	h.HandleOpenAIChat(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Result().StatusCode)
	}

	var resp oaiCompletion
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hello world" {
		t.Errorf("unexpected response content: %+v", resp.Choices)
	}
}

func TestOpenAIChat_Stream(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	registry.RegisterWithRole("mock", "mock", "default", &mockStreamProvider{
		chunks: []types.StreamEvent{
			{Type: types.StreamTextDelta, Content: "chunk1"},
			{Type: types.StreamTextDelta, Content: "chunk2"},
		},
	})

	router := llm.NewInferenceRouter(registry, nil)
	h := &SysAdminHandler{Registry: registry, Router: router}

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	h.HandleOpenAIChat(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Result().StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "chunk1") || !strings.Contains(body, "chunk2") || !strings.Contains(body, "[DONE]") {
		t.Errorf("unexpected stream output: %s", body)
	}
}

// TestOpenAIChat_Sync_StreamCancelled 复现 empty_response 同族根因在 OpenAI 兼容
// 接口非流式路径的版本：流未产出任何内容即被中断（ctx 超时/预算守卫硬阻断）时
// 必须返回错误状态码，不能返回 200 + 空 content——外部客户端会把它当成一次
// "正常但空"的生成结果，无法区分真实空回复与服务端故障。
func TestOpenAIChat_Sync_StreamCancelled(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	registry.RegisterWithRole("mock", "mock", "default", &mockStreamProvider{
		chunks: []types.StreamEvent{
			{Type: types.StreamCancelled, Content: "context deadline exceeded"},
		},
	})

	router := llm.NewInferenceRouter(registry, nil)
	h := &SysAdminHandler{Registry: registry, Router: router}

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	h.HandleOpenAIChat(w, req)

	if w.Result().StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 on cancelled stream, got 200 body=%s", w.Body.String())
	}
}

// TestOpenAIChat_Stream_StreamCancelled 同上，流式 SSE 路径版本：中断事件必须
// 携带非 "stop" 的 finish_reason，不能让客户端把中断误判为正常生成完毕。
func TestOpenAIChat_Stream_StreamCancelled(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	registry.RegisterWithRole("mock", "mock", "default", &mockStreamProvider{
		chunks: []types.StreamEvent{
			{Type: types.StreamCancelled, Content: "context deadline exceeded"},
		},
	})

	router := llm.NewInferenceRouter(registry, nil)
	h := &SysAdminHandler{Registry: registry, Router: router}

	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	h.HandleOpenAIChat(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `data: {"error":{"message":"Inference request timed out","type":"server_error"}}`) {
		t.Errorf("expected OpenAI SSE error object: %s", body)
	}
	if strings.Contains(body, "context deadline exceeded") || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "[error:") {
		t.Errorf("stream failure leaked cause or looked like a successful content chunk: %s", body)
	}
}

func TestOpenAIChat_StreamFailureUsesProtocolErrorEvent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider *mockStreamProvider
		timeout  time.Duration
		cause    string
	}{
		{name: "router error", provider: &mockStreamProvider{err: errors.New("private-router-cause")}, cause: "private-router-cause"},
		{name: "stream error", provider: &mockStreamProvider{chunks: []types.StreamEvent{{Type: types.StreamError, Content: "private-stream-cause"}}}, cause: "private-stream-cause"},
		{name: "empty stream error", provider: &mockStreamProvider{chunks: []types.StreamEvent{{Type: types.StreamError}}}},
		{name: "cancelled", provider: &mockStreamProvider{chunks: []types.StreamEvent{{Type: types.StreamCancelled, Content: "private-cancel-cause"}}}, cause: "private-cancel-cause"},
		{name: "idle timeout", provider: &mockStreamProvider{hold: true}, timeout: time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
			registry.RegisterWithRole("mock", "mock", "default", tc.provider)
			h := &SysAdminHandler{Registry: registry, Router: llm.NewInferenceRouter(registry, nil), StreamIdleTimeout: tc.timeout}
			reqBody := `{"messages":[{"role":"user","content":"hi"}],"stream":true}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChat(recorder, req)

			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("stream headers changed after submission: status=%d content-type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
			}
			if !strings.Contains(body, `data: {"error":{"message":"Inference request timed out"`) &&
				!strings.Contains(body, `data: {"error":{"message":"Inference request failed"`) {
				t.Fatalf("missing protocol error event: %s", body)
			}
			if !strings.Contains(body, "data: [DONE]\n\n") || strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "[error:") {
				t.Fatalf("failure looked like a successful content stream: %s", body)
			}
			if tc.cause != "" && strings.Contains(body, tc.cause) {
				t.Fatalf("upstream cause leaked into stream: %s", body)
			}
		})
	}
}

func TestOpenAIChatSync_StreamErrorAlwaysFailsButEmptyStreamSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		events     []types.StreamEvent
		wantStatus int
		wantError  bool
	}{
		{name: "empty stream error", events: []types.StreamEvent{{Type: types.StreamError}}, wantStatus: http.StatusInternalServerError, wantError: true},
		{name: "non-empty stream error", events: []types.StreamEvent{{Type: types.StreamError, Content: "private-stream-cause"}}, wantStatus: http.StatusInternalServerError, wantError: true},
		{name: "empty successful stream", events: nil, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
			registry.RegisterWithRole("mock", "mock", "default", &mockStreamProvider{chunks: tc.events})
			h := &SysAdminHandler{Registry: registry, Router: llm.NewInferenceRouter(registry, nil)}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}],"stream":false}`))
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChat(recorder, req)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantError {
				var body struct {
					Error struct {
						Message string `json:"message"`
						Type    string `json:"type"`
					} `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
					t.Fatalf("expected OpenAI JSON error: %v; body=%s", err, recorder.Body.String())
				}
				if body.Error.Type != "server_error" || body.Error.Message != "Inference request failed" || strings.Contains(recorder.Body.String(), "private-stream-cause") {
					t.Fatalf("unexpected or unsafe error envelope: %+v body=%s", body.Error, recorder.Body.String())
				}
				return
			}
			var body oaiCompletion
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("empty successful response lost completion semantics: %v; body=%s", err, recorder.Body.String())
			}
			if len(body.Choices) != 1 || body.Choices[0].Message == nil || body.Choices[0].Message.Content != "" {
				t.Fatalf("unexpected empty completion: %+v", body)
			}
		})
	}
}

func TestOpenAIChat_Errors(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	router := llm.NewInferenceRouter(registry, nil)
	h := &SysAdminHandler{Registry: registry, Router: router}

	// Invalid JSON
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{invalid`))
	w := httptest.NewRecorder()
	h.HandleOpenAIChat(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400")
	}
	var invalidBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &invalidBody); err != nil {
		t.Fatalf("invalid JSON error must retain the OpenAI envelope: %v body=%s", err, w.Body.String())
	}
	if _, ok := invalidBody["error"]; !ok {
		t.Fatalf("invalid JSON error envelope missing error object: %+v", invalidBody)
	}

	// Empty messages
	req = httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"messages":[]}`))
	w = httptest.NewRecorder()
	h.HandleOpenAIChat(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400")
	}

	// No provider configured
	reqBody := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req = httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	w = httptest.NewRecorder()
	h.HandleOpenAIChat(w, req)
	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Result().StatusCode)
	}
}
