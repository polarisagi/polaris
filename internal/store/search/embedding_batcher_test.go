package search

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func makeEmbedFn(vecs [][]float32, err error) EmbedFn {
	return func(_ context.Context, texts []string, _ string) ([][]float32, error) {
		if err != nil {
			return nil, err
		}
		result := make([][]float32, len(texts))
		for i := range texts {
			if i < len(vecs) {
				result[i] = vecs[i]
			}
		}
		return result, nil
	}
}

func TestNewEmbeddingBatcher_Defaults(t *testing.T) {
	b := NewEmbeddingBatcher(0, 0, nil)
	if b.batchWindow != 10*time.Millisecond {
		t.Errorf("expected 10ms default batchWindow, got %v", b.batchWindow)
	}
	if b.maxBatchSize != 100 {
		t.Errorf("expected 100 default maxBatchSize, got %d", b.maxBatchSize)
	}
}

func TestFlushBatch_NilEmbedFn(t *testing.T) {
	b := NewEmbeddingBatcher(10*time.Millisecond, 100, nil)
	_, err := b.flushBatch(context.Background(), []string{"hello"}, "text-emb-3")
	if err == nil {
		t.Fatal("expected error for nil embedFn")
	}
	var pe *apperr.Error
	if e, ok := err.(*apperr.Error); ok {
		pe = e
	}
	if pe == nil || pe.Code != apperr.CodeInternal {
		t.Errorf("expected CodeInternal, got: %v", err)
	}
}

func TestFlushBatch_Success(t *testing.T) {
	vecs := [][]float32{{0.1, 0.2}, {0.3, 0.4}}
	b := NewEmbeddingBatcher(10*time.Millisecond, 100, makeEmbedFn(vecs, nil))

	results, err := b.flushBatch(context.Background(), []string{"hello", "world"}, "text-emb-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Vector[0] != 0.1 || results[1].Vector[0] != 0.3 {
		t.Errorf("wrong vectors: %v", results)
	}
}

func TestFlushBatch_APIError(t *testing.T) {
	apiErr := apperr.New(apperr.CodeInternal, "rate limit")
	b := NewEmbeddingBatcher(10*time.Millisecond, 100, makeEmbedFn(nil, apiErr))

	_, err := b.flushBatch(context.Background(), []string{"text"}, "text-emb-3")
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	var pe *apperr.Error
	if e, ok := err.(*apperr.Error); ok {
		pe = e
	}
	if pe == nil || pe.Code != apperr.CodeInternal {
		t.Errorf("expected CodeInternal, got: %v", err)
	}
	if pe.Cause == nil || !strings.Contains(pe.Cause.Error(), "rate limit") {
		t.Errorf("expected wrapped cause, got: %v", pe.Cause)
	}
}

func TestEmbed_LargeDirectFlush(t *testing.T) {
	vecs := make([][]float32, 100)
	for i := range vecs {
		vecs[i] = []float32{float32(i)}
	}
	b := NewEmbeddingBatcher(10*time.Millisecond, 5, makeEmbedFn(vecs, nil))

	texts := make([]string, 5) // len=5 >= maxBatchSize(5) → direct flush
	for i := range texts {
		texts[i] = "text"
	}
	results, err := b.Embed(context.Background(), texts, "text-emb-3", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("expected 5 results, got %d", len(results))
	}
}
