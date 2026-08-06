package search

import (
	"context"
	"errors"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// stubSource 可控的 DocumentSource 桩。
type stubSource struct {
	bm25    []types.ScoredFragment
	vector  []types.ScoredFragment
	graph   []types.ScoredFragment
	bm25Err error
	vecErr  error
	// bm25Calls 记录 BM25 被调用次数（验证融合层不会重复触发召回）。
	bm25Calls int
}

func (s *stubSource) SearchBM25(_ context.Context, _ string, _ int) ([]types.ScoredFragment, error) {
	s.bm25Calls++
	return s.bm25, s.bm25Err
}

func (s *stubSource) SearchVector(_ context.Context, _ []float32, _ int) ([]types.ScoredFragment, error) {
	return s.vector, s.vecErr
}

func (s *stubSource) SearchGraph(_ context.Context, _ string, _ int) ([]types.ScoredFragment, error) {
	return s.graph, nil
}

func frag(src string) types.ScoredFragment {
	return types.ScoredFragment{Source: src, Content: src}
}

// TestHybridSearch_ZeroWeightDisablesPath 守护 M05 §12.3 漂移降级：
// VectorWeight 显式置 0 必须真正关闭向量路，不得被"默认值"复活。
// 这是本文件初版的实际缺陷（`if vw <= 0 { vw = 0.6 }`），一旦回归，
// Embedding 漂移降级开关会静默失效且无任何报错。
func TestHybridSearch_ZeroWeightDisablesPath(t *testing.T) {
	src := &stubSource{
		bm25:   []types.ScoredFragment{frag("a")},
		vector: []types.ScoredFragment{frag("v1"), frag("v2")},
	}
	got, err := HybridSearch(context.Background(), src, "q", []float32{1, 2, 3}, HybridSearchConfig{
		BM25Weight:   1.0,
		VectorWeight: 0, // 显式关闭
		GraphWeight:  0,
		TopK:         10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range got {
		if f.Source == "v1" || f.Source == "v2" {
			t.Fatalf("vector path must be disabled when VectorWeight==0, got %q", f.Source)
		}
		if f.ExplainBits&types.BitVector != 0 {
			t.Fatalf("disabled path must not set BitVector, got bits=%08b", f.ExplainBits)
		}
	}
}

// TestHybridSearch_PreservesInputOrderOnTiedScores 守护 Knowledge 的召回次序：
// Chunk 召回结果 Score 全为 0，排名完全依赖调用方给定的顺序（FTS rank /
// 向量相似度序）。非稳定排序会把它打成随机排列，检索质量塌方且无声无息。
func TestHybridSearch_PreservesInputOrderOnTiedScores(t *testing.T) {
	src := &stubSource{
		bm25: []types.ScoredFragment{frag("c1"), frag("c2"), frag("c3"), frag("c4"), frag("c5")},
	}
	want := []string{"c1", "c2", "c3", "c4", "c5"}
	// 多跑几轮：map 迭代顺序随机，单次通过可能是巧合。
	for range 20 {
		got, err := HybridSearch(context.Background(), src, "q", nil, HybridSearchConfig{
			BM25Weight: 1.0,
			TopK:       5,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("want %d results, got %d", len(want), len(got))
		}
		for i, w := range want {
			if got[i].Source != w {
				t.Fatalf("rank %d: want %q, got %q (full=%v)", i, w, got[i].Source, sources(got))
			}
		}
	}
}

// TestHybridSearch_BM25ErrorAborts 主路失败必须中止（与重构前 knowledge
// `ftsErr != nil → return` 一致），不得静默返回残缺结果。
func TestHybridSearch_BM25ErrorAborts(t *testing.T) {
	sentinel := errors.New("fts down")
	src := &stubSource{bm25Err: sentinel, vector: []types.ScoredFragment{frag("v1")}}
	_, err := HybridSearch(context.Background(), src, "q", []float32{1}, HybridSearchConfig{
		BM25Weight: 1.0, VectorWeight: 1.0, TopK: 5,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want bm25 error propagated, got %v", err)
	}
}

// TestHybridSearch_VectorErrorDegrades 辅路失败只降级：向量路挂掉时
// BM25 结果仍须返回（重构前 memory/knowledge 对该路均为静默降级）。
func TestHybridSearch_VectorErrorDegrades(t *testing.T) {
	src := &stubSource{
		bm25:   []types.ScoredFragment{frag("a")},
		vecErr: errors.New("vector backend down"),
	}
	got, err := HybridSearch(context.Background(), src, "q", []float32{1}, HybridSearchConfig{
		BM25Weight: 1.0, VectorWeight: 1.0, TopK: 5,
	})
	if err != nil {
		t.Fatalf("vector failure must degrade, not abort: %v", err)
	}
	if len(got) != 1 || got[0].Source != "a" {
		t.Fatalf("want BM25 result preserved, got %v", sources(got))
	}
}

// TestHybridSearch_RRFWeightsRankCrossPaths 交叉验证 RRF 权重生效：
// 高权重路的首位应压过低权重路的首位。
func TestHybridSearch_RRFWeightsRankCrossPaths(t *testing.T) {
	src := &stubSource{
		bm25:   []types.ScoredFragment{frag("bm-top")},
		vector: []types.ScoredFragment{frag("vec-top")},
	}
	got, err := HybridSearch(context.Background(), src, "q", []float32{1}, HybridSearchConfig{
		BM25Weight:   0.3, // M10 §2.2 权重形态：向量为主
		VectorWeight: 0.6,
		TopK:         5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 || got[0].Source != "vec-top" {
		t.Fatalf("higher-weighted path should rank first, got %v", sources(got))
	}
}

// TestHybridSearch_TopKZeroMeansNoTruncation TopK<=0 表示不截断，
// 领域默认值由调用方解析（memory=20 / knowledge=10）。
func TestHybridSearch_TopKZeroMeansNoTruncation(t *testing.T) {
	src := &stubSource{bm25: []types.ScoredFragment{frag("a"), frag("b"), frag("c")}}
	got, err := HybridSearch(context.Background(), src, "q", nil, HybridSearchConfig{
		BM25Weight: 1.0, TopK: 0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want no truncation, got %d results", len(got))
	}
}

func sources(fs []types.ScoredFragment) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Source
	}
	return out
}
