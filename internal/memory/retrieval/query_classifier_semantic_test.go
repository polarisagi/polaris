package retrieval

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
	classifier := NewSemanticQueryClassifier()
	err := classifier.InitPrototypes(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	classifier.mu.RLock()
	defer classifier.mu.RUnlock()
	if len(classifier.vecs) != 0 {
		t.Fatalf("expected empty vecs, got %v", len(classifier.vecs))
	}
}

func TestClassifyQuerySemantic_FallbackOnNilEmbedder(t *testing.T) {
	classifier := NewSemanticQueryClassifier()
	classifier.mu.Lock()
	classifier.vecs = nil
	classifier.mu.Unlock()

	ctx := context.Background()
	qt := classifier.ClassifyQuerySemantic(ctx, "最近发生了什么", nil)
	if qt != QueryTypeTemporal {
		t.Fatalf("expected QueryTypeTemporal, got %v", qt)
	}
}

func TestClassifyQuerySemantic_LowConfidence(t *testing.T) {
	classifier := NewSemanticQueryClassifier()
	classifier.mu.Lock()
	classifier.vecs = map[QueryType][]float32{
		QueryTypeTemporal: {1, 0},
	}
	classifier.mu.Unlock()

	ctx := context.Background()
	me := &mockEmbedder{vec: []float32{0, 1}} // orthogonal, sim = 0 < 0.3
	qt := classifier.ClassifyQuerySemantic(ctx, "测试低置信度", me)
	if qt != QueryTypeUnknown {
		t.Fatalf("expected QueryTypeUnknown, got %v", qt)
	}
}

func TestClassifyQuerySemantic_HighConfidence(t *testing.T) {
	classifier := NewSemanticQueryClassifier()
	classifier.mu.Lock()
	classifier.vecs = map[QueryType][]float32{
		QueryTypeTemporal: {1, 0},
		QueryTypeFactual:  {0, 1},
	}
	classifier.mu.Unlock()

	ctx := context.Background()
	me := &mockEmbedder{vec: []float32{0.9, 0.1}} // highly similar to temporal
	qt := classifier.ClassifyQuerySemantic(ctx, "这是一个高置信度的时间查询吗", me)
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
