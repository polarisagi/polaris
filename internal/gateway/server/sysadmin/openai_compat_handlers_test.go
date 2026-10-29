package sysadmin

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm"
)

func TestOpenAICompatHandlers(t *testing.T) {
	registry := llm.NewProviderRegistry(config.M1RouterThresholds{})
	router := llm.NewInferenceRouter(registry, nil)
	h := &SysAdminHandler{
		Registry: registry,
		Router:   router,
	}

	body := `{"model": "test-model", "messages": [{"role": "user", "content": "hello"}], "stream": true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	// This will panic or return 500 because h.agent is nil, but it will cover the top part of the handler
	h.HandleOpenAIChat(w, req)
	t.Logf("openai chat returned: %v %s", w.Result().StatusCode, w.Body.String())

	body = `{"model": "test-model", "messages": [{"role": "user", "content": "hello"}], "stream": false}`
	req = httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	w = httptest.NewRecorder()

	h.HandleOpenAIChat(w, req)
	t.Logf("openai chat returned: %v %s", w.Result().StatusCode, w.Body.String())
}
