package chat

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSSEHandlers(t *testing.T) {
	h := &ChatHandler{}

	req := httptest.NewRequest("GET", "/api/v1/agent/stream?session_id=123", nil)
	w := httptest.NewRecorder()

	h.HandleAgentStream(w, req)
	// It should fail or return 500/400 because no db/agent is set, but it hits the handler.
	if w.Result().StatusCode == http.StatusNotFound {
		t.Errorf("expected handler to be hit, got 404")
	}
}
