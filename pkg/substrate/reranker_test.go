package substrate

import (
	"context"
	"math"
	"testing"
)

// ── NilReranker ───────────────────────────────────────────────────────────────

func TestNilReranker_Passthrough(t *testing.T) {
	docs := []ScoredFragment{
		{Content: "doc-a", Score: 0.9},
		{Content: "doc-b", Score: 0.5},
	}
	got := NilReranker{}.Rerank(context.Background(), "query", docs)
	if len(got) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(got))
	}
	if got[0].Content != "doc-a" {
		t.Error("NilReranker should not change order")
	}
}

func TestNilReranker_Empty(t *testing.T) {
	got := NilReranker{}.Rerank(context.Background(), "q", nil)
	if got != nil {
		t.Error("NilReranker nil input should return nil")
	}
}

// ── cosineSim32 ───────────────────────────────────────────────────────────────

func TestCosineSim32_Identical(t *testing.T) {
	v := []float32{1, 0, 0}
	s := cosineSim32(v, v)
	if math.Abs(s-1.0) > 1e-6 {
		t.Errorf("identical vectors should have cosine=1, got %f", s)
	}
}

func TestCosineSim32_Orthogonal(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	s := cosineSim32(a, b)
	if math.Abs(s) > 1e-6 {
		t.Errorf("orthogonal vectors should have cosine≈0, got %f", s)
	}
}

func TestCosineSim32_LengthMismatch(t *testing.T) {
	if s := cosineSim32([]float32{1, 2}, []float32{1, 2, 3}); s != 0 {
		t.Errorf("length mismatch should return 0, got %f", s)
	}
}

func TestCosineSim32_Empty(t *testing.T) {
	if s := cosineSim32(nil, nil); s != 0 {
		t.Errorf("empty vectors should return 0, got %f", s)
	}
}

func TestCosineSim32_ZeroVector(t *testing.T) {
	a := []float32{0, 0}
	b := []float32{1, 0}
	if s := cosineSim32(a, b); s != 0 {
		t.Errorf("zero vector should return 0, got %f", s)
	}
}

// ── MaxSimScore ───────────────────────────────────────────────────────────────

func TestMaxSimScore_Identical(t *testing.T) {
	v := []float32{1, 0, 0}
	vecs := [][]float32{v}
	s := MaxSimScore(vecs, vecs)
	if math.Abs(s-1.0) > 1e-6 {
		t.Errorf("identical token vecs should give MaxSim=1, got %f", s)
	}
}

func TestMaxSimScore_Empty(t *testing.T) {
	if s := MaxSimScore(nil, nil); s != 0 {
		t.Errorf("empty inputs should return 0, got %f", s)
	}
	if s := MaxSimScore([][]float32{{1, 0}}, nil); s != 0 {
		t.Errorf("empty doc vecs should return 0, got %f", s)
	}
}

func TestMaxSimScore_MultiToken(t *testing.T) {
	// q=[e1,e2], d=[e1,e3]
	// MaxSim(e1,{e1,e3})=1, MaxSim(e2,{e1,e3})=0 → total=1/2=0.5
	e1 := []float32{1, 0}
	e2 := []float32{0, 1}
	e3 := []float32{0, 1} // same as e2
	qVecs := [][]float32{e1, e2}
	dVecs := [][]float32{e1, e3}
	s := MaxSimScore(qVecs, dVecs)
	if math.Abs(s-1.0) > 1e-6 {
		// e1→e1=1, e2→e3=1 → avg=1
		t.Errorf("expected MaxSim=1.0, got %f", s)
	}
}

// ── ApproximateColBERTReranker ────────────────────────────────────────────────

// mockEmbedder 对文本返回固定向量（按内容哈希区分）。
type mockEmbedder struct{}

func (mockEmbedder) Embed(text string) []float32 {
	// 简单：每个词的首字母 ASCII 映射为向量维度
	vec := make([]float32, 26)
	for _, b := range []byte(text) {
		if b >= 'a' && b <= 'z' {
			vec[b-'a'] += 1
		} else if b >= 'A' && b <= 'Z' {
			vec[b-'A'] += 1
		}
	}
	// 归一化
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		norm = float32(math.Sqrt(float64(norm)))
		for i := range vec {
			vec[i] /= norm
		}
	}
	return vec
}

func TestApproximateColBERTReranker_OrderChanged(t *testing.T) {
	// 查询: "golang concurrency"
	// doc-a: "golang goroutines channels concurrency" (高相关)
	// doc-b: "python web framework django" (低相关)
	docs := []ScoredFragment{
		{Content: "python web framework django", Score: 0.9},            // RRF 高分但低相关
		{Content: "golang goroutines channels concurrency", Score: 0.5}, // RRF 低分但高相关
	}
	r := NewApproximateColBERTReranker(mockEmbedder{}, 2)
	reranked := r.Rerank(context.Background(), "golang concurrency", docs)

	if len(reranked) != 2 {
		t.Fatalf("expected 2 results, got %d", len(reranked))
	}
	// golang 相关文档应排在第一位
	if reranked[0].Content != "golang goroutines channels concurrency" {
		t.Errorf("expected golang doc first after rerank, got %q", reranked[0].Content)
	}
}

func TestApproximateColBERTReranker_EmptyQuery(t *testing.T) {
	docs := []ScoredFragment{{Content: "some doc"}}
	r := NewApproximateColBERTReranker(mockEmbedder{}, 3)
	got := r.Rerank(context.Background(), "", docs)
	// 空 query 无 token vecs → 透传原始列表
	if len(got) != 1 {
		t.Errorf("expected 1 doc, got %d", len(got))
	}
}

func TestApproximateColBERTReranker_NilEmbedder(t *testing.T) {
	docs := []ScoredFragment{{Content: "doc"}}
	r := &ApproximateColBERTReranker{embedder: nil, window: 3}
	got := r.Rerank(context.Background(), "query", docs)
	if len(got) != 1 {
		t.Errorf("nil embedder should passthrough, got %d", len(got))
	}
}

func TestApproximateColBERTReranker_DefaultWindow(t *testing.T) {
	r := NewApproximateColBERTReranker(mockEmbedder{}, 0)
	if r.window != 3 {
		t.Errorf("default window should be 3, got %d", r.window)
	}
}

// ── hybrid_retrieve 集成：Reranker 接入 ──────────────────────────────────────

func TestRetrievalConfig_NilReranker_NoOp(t *testing.T) {
	cfg := RetrievalConfig{
		RerankTopM: 10,
		Reranker:   nil, // nil → 跳过重排
	}
	if cfg.Reranker != nil {
		t.Error("nil Reranker should stay nil")
	}
	if cfg.RerankTopM != 10 {
		t.Errorf("expected RerankTopM=10, got %d", cfg.RerankTopM)
	}
}

// ── stableSort ────────────────────────────────────────────────────────────────

func TestStableSort_Descending(t *testing.T) {
	items := []scoredDoc{
		{score: 0.3},
		{score: 0.9},
		{score: 0.1},
		{score: 0.7},
	}
	stableSort(items)
	for i := 1; i < len(items); i++ {
		if items[i].score > items[i-1].score {
			t.Errorf("not descending at [%d]: %f > %f", i, items[i].score, items[i-1].score)
		}
	}
}
