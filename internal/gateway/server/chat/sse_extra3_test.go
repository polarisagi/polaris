package chat

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestHandleAgentStreamErrors(t *testing.T) {
	defer func() { recover() }()
	h := &ChatHandler{DataDir: t.TempDir()}

	// Bad JSON
	req1 := httptest.NewRequest("POST", "/agent/stream", bytes.NewBufferString("invalid json"))
	w1 := httptest.NewRecorder()
	h.HandleAgentStream(w1, req1)

	// Empty input
	req2 := httptest.NewRequest("POST", "/agent/stream", bytes.NewBufferString(`{"input": "   ", "session_id": "123"}`))
	w2 := httptest.NewRecorder()
	h.HandleAgentStream(w2, req2)

	// Flusher test
	req3 := httptest.NewRequest("POST", "/agent/stream", bytes.NewBufferString(`{"input": "hello", "session_id": "123"}`))
	w3 := httptest.NewRecorder()
	h.HandleAgentStream(w3, req3)
}
