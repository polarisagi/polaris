package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddingRebenchEndpoint(t *testing.T) {
	srv := &Server{}

	// Case 1: no rebencher configured
	req := httptest.NewRequest(http.MethodPost, "/v1/embedding/rebench", nil)
	w := httptest.NewRecorder()
	srv.handleEmbeddingRebench(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if resp["status"] != "no_rebencher" {
		t.Fatalf("expected no_rebencher, got %s", resp["status"])
	}

	// Case 2: rebencher configured
	called := false
	srv.SetEmbeddingRebencher(func(ctx context.Context) error {
		called = true
		return nil
	})

	req2 := httptest.NewRequest(http.MethodPost, "/v1/embedding/rebench", nil)
	w2 := httptest.NewRecorder()
	srv.handleEmbeddingRebench(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w2.Code)
	}
	var resp2 map[string]string
	if err := json.NewDecoder(w2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if resp2["status"] != "rebench_started" {
		t.Fatalf("expected rebench_started, got %s", resp2["status"])
	}
	if !called {
		t.Fatal("expected rebench callback to be called")
	}

	// Case 3: Method Not Allowed
	req3 := httptest.NewRequest(http.MethodGet, "/v1/embedding/rebench", nil)
	w3 := httptest.NewRecorder()
	srv.handleEmbeddingRebench(w3, req3)
	if w3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", w3.Code)
	}
}

func TestEmbeddingStatusReporting(t *testing.T) {
	srv := &Server{}
	st := srv.GetEmbeddingStatus()
	if st.Backend != "none" || st.State != "fts" {
		t.Fatalf("default embedding status mismatch: %+v", st)
	}

	srv.SetEmbeddingStatusProvider(func() EmbeddingStatus {
		return EmbeddingStatus{
			Backend: "onnx",
			Model:   "onnx:bge-small-zh-v1.5-int8@512",
			Dim:     512,
			State:   "ready",
		}
	})

	st2 := srv.GetEmbeddingStatus()
	if st2.Backend != "onnx" || st2.Model != "onnx:bge-small-zh-v1.5-int8@512" || st2.Dim != 512 || st2.State != "ready" {
		t.Fatalf("updated embedding status mismatch: %+v", st2)
	}
}
