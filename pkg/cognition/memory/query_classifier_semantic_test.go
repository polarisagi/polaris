package memory

import (
	"context"
	"testing"
)

type mockEmbedder struct {
	vec []float32
	err error
}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return m.vec, m.err
}

func TestInitPrototypes_NilEmbedder(t *testing.T) {
	err := InitPrototypes(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	globalPrototypes.mu.RLock()
	defer globalPrototypes.mu.RUnlock()
	if len(globalPrototypes.vecs) != 0 {
		t.Fatalf("expected empty vecs, got %v", len(globalPrototypes.vecs))
	}
}

func TestClassifyQuerySemantic_FallbackOnNilEmbedder(t *testing.T) {
	globalPrototypes.mu.Lock()
	globalPrototypes.vecs = nil
	globalPrototypes.mu.Unlock()

	ctx := context.Background()
	qt := ClassifyQuerySemantic(ctx, "最近发生了什么", nil)
	if qt != QueryTypeTemporal {
		t.Fatalf("expected QueryTypeTemporal, got %v", qt)
	}
}

func TestClassifyQuerySemantic_LowConfidence(t *testing.T) {
	globalPrototypes.mu.Lock()
	globalPrototypes.vecs = map[QueryType][]float32{
		QueryTypeTemporal: {1, 0},
	}
	globalPrototypes.mu.Unlock()

	ctx := context.Background()
	me := &mockEmbedder{vec: []float32{0, 1}} // orthogonal, sim = 0 < 0.3
	qt := ClassifyQuerySemantic(ctx, "测试低置信度", me)
	if qt != QueryTypeUnknown {
		t.Fatalf("expected QueryTypeUnknown, got %v", qt)
	}
}

func TestClassifyQuerySemantic_HighConfidence(t *testing.T) {
	globalPrototypes.mu.Lock()
	globalPrototypes.vecs = map[QueryType][]float32{
		QueryTypeTemporal: {1, 0},
		QueryTypeFactual:  {0, 1},
	}
	globalPrototypes.mu.Unlock()

	ctx := context.Background()
	me := &mockEmbedder{vec: []float32{0.9, 0.1}} // highly similar to temporal
	qt := ClassifyQuerySemantic(ctx, "这是一个高置信度的时间查询吗", me)
	if qt != QueryTypeTemporal {
		t.Fatalf("expected QueryTypeTemporal, got %v", qt)
	}
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	sim := cosineSimilarity([]float32{0, 0}, []float32{1, 1})
	if sim != 0 {
		t.Fatalf("expected 0, got %v", sim)
	}
	sim = cosineSimilarity([]float32{1, 1}, []float32{0, 0})
	if sim != 0 {
		t.Fatalf("expected 0, got %v", sim)
	}
	sim = cosineSimilarity(nil, []float32{1, 1})
	if sim != 0 {
		t.Fatalf("expected 0, got %v", sim)
	}
}
