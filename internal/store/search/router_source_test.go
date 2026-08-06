package search

import (
	"math"
	"testing"
)

// TestCosineF32F64 Tier-0 向量召回的相似度计算：查询向量是 float32、
// 文档向量在 JSON 里是 float64，跨类型比对必须正确且对退化输入安全。
func TestCosineF32F64(t *testing.T) {
	cases := []struct {
		name string
		q    []float32
		d    []float64
		want float64
		ok   bool
	}{
		{"identical", []float32{1, 0, 0}, []float64{1, 0, 0}, 1.0, true},
		{"orthogonal", []float32{1, 0}, []float64{0, 1}, 0.0, true},
		{"opposite", []float32{1, 0}, []float64{-1, 0}, -1.0, true},
		// 维度不一致：必须跳过该文档而非静默按前 N 维比对——
		// 那会让换嵌入模型后的旧向量以错误的相似度混进召回。
		{"dim mismatch", []float32{1, 0, 0}, []float64{1, 0}, 0, false},
		{"empty query", nil, []float64{1, 0}, 0, false},
		{"zero doc vector", []float32{1, 0}, []float64{0, 0}, 0, false},
		{"zero query vector", []float32{0, 0}, []float64{1, 0}, 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := cosineF32F64(c.q, c.d)
			if ok != c.ok {
				t.Fatalf("ok: want %v, got %v", c.ok, ok)
			}
			if ok && math.Abs(got-c.want) > 1e-6 {
				t.Fatalf("similarity: want %v, got %v", c.want, got)
			}
		})
	}
}

// TestRouterDocumentSource_GraphReturnsEmptyNotError StorageRouter 路径没有图
// 遍历能力。按 DocumentSource 契约必须返回 (nil, nil) 而非错误——
// 返回错误会被融合层记为一路失败并打 Warn，把"本就不支持"误报成"出故障了"。
func TestRouterDocumentSource_GraphReturnsEmptyNotError(t *testing.T) {
	s := &routerDocumentSource{}
	got, err := s.SearchGraph(t.Context(), "q", 10)
	if err != nil {
		t.Fatalf("unsupported path must return nil error, got %v", err)
	}
	if got != nil {
		t.Fatalf("unsupported path must return nil results, got %v", got)
	}
}

// TestHybridSearchEngine_EmbedQueryNilEmbedder 纯 FTS 降级（Tier-0 未注入
// embedder）时查询编码返回 nil，融合层据此跳过向量路。
func TestHybridSearchEngine_EmbedQueryNilEmbedder(t *testing.T) {
	e := NewHybridSearchEngine(nil, nil)
	if got := e.EmbedQuery("hello"); got != nil {
		t.Fatalf("nil embedder must yield nil query vector, got %v", got)
	}
	if got := e.EmbedQuery(""); got != nil {
		t.Fatalf("empty query must yield nil query vector, got %v", got)
	}
}
