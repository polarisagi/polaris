package sysadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockStreamProvider struct {
	protocol.Provider
	chunks []types.StreamEvent
	err    error
}

func (m *mockStreamProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	if m.err != nil {
		return nil, m.err
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

func TestOpenAIChat_Sync(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	registry.RegisterWithRole("mock", "mock", "default", &mockStreamProvider{
		chunks: []types.StreamEvent{
			{Type: types.StreamTextDelta, Content: "Hello"},
			{Type: types.StreamTextDelta, Content: " world"},
		},
	})

	h := &SysAdminHandler{Registry: registry}

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

	h := &SysAdminHandler{Registry: registry}

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

func TestOpenAIChat_Errors(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	h := &SysAdminHandler{Registry: registry}

	// Invalid JSON
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{invalid`))
	w := httptest.NewRecorder()
	h.HandleOpenAIChat(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400")
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
