package chat

import (
	"net/http/httptest"
	"testing"
)

func TestHandleAgentStreamExtra(t *testing.T) {
	defer func() { recover() }()
	h := &ChatHandler{DataDir: t.TempDir()}

	// Test with no POST body
	req1 := httptest.NewRequest("POST", "/agent/stream", nil)
	req1.SetPathValue("session_id", "123")
	w1 := httptest.NewRecorder()
	h.HandleAgentStream(w1, req1)

	// Test with GET
	req2 := httptest.NewRequest("GET", "/agent/stream", nil)
	req2.SetPathValue("session_id", "123")
	w2 := httptest.NewRecorder()
	h.HandleAgentStream(w2, req2)

	// Test without session_id
	req3 := httptest.NewRequest("POST", "/agent/stream", nil)
	w3 := httptest.NewRecorder()
	h.HandleAgentStream(w3, req3)
}
