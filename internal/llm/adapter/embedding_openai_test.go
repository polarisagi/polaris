package adapter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func TestOpenAICompatibleEmbedding(t *testing.T) {
	client := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			if req.Method != http.MethodPost || req.URL.Path != "/embeddings" {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(bytes.NewReader(nil)),
					Header:     make(http.Header),
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader([]byte(`{
					"data": [
						{
							"index": 0,
							"embedding": [0.1, 0.2, 0.3]
						}
					]
				}`))),
				Header: make(http.Header),
			}
		}),
	}

	adapter := NewOpenAICompatibleEmbeddingAdapter("http://dummy", "test-model", []byte("test-key"), client)

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

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}
