package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAICompatibleEmbedding(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": [
				{
					"index": 0,
					"embedding": [0.1, 0.2, 0.3]
				}
			]
		}`))
	}))
	defer ts.Close()

	adapter := NewOpenAICompatibleEmbeddingAdapter(ts.URL, "test-model", []byte("test-key"), ts.Client())

	// Test Embed
	vec := adapter.Embed("test text")
	if len(vec) != 3 || vec[0] != 0.1 || vec[1] != 0.2 || vec[2] != 0.3 {
		t.Fatalf("unexpected embed result: %v", vec)
	}

	// Test EmbedBatch
	vecs, err := adapter.EmbedBatch(context.Background(), []string{"test text"})
	if err != nil {
		t.Fatalf("EmbedBatch error: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Fatalf("unexpected EmbedBatch result length")
	}
}
