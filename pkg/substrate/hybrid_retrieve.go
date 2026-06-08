package substrate

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// HybridSearchEngine 提供统一接口: Search(ctx, query, scope, config) → []ScoredFragment
type HybridSearchEngine struct {
	router   *StorageRouter
	embedder Embedder
}

func NewHybridSearchEngine(router *StorageRouter, embedder Embedder) *HybridSearchEngine {
	return &HybridSearchEngine{
		router:   router,
		embedder: embedder,
	}
}

func (e *HybridSearchEngine) Search(ctx context.Context, query string, scope []byte, config RetrievalConfig) ([]ScoredFragment, error) { //nolint:gocyclo,nestif
	if query == "" {
		return nil, perrors.New(perrors.CodeInvalidInput, "empty query")
	}

	ftsStore := e.router.Route(ctx, &StorageRequest{
		DataType:   "knowledge",
		AccessMode: "adhoc_query",
	})
	vecStore := e.router.Route(ctx, &StorageRequest{
		DataType:   "knowledge",
		AccessMode: "knn_read",
	})

	var ftsResults []ScoredFragment
	ftsIter, err := ftsStore.Scan(ctx, scope)
	if err == nil {
		defer ftsIter.Close()
		for ftsIter.Next() {
			var c struct {
				ID      string `json:"id"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(ftsIter.Value(), &c); err == nil {
				score := bm25Score(c.Content, query)
				if score > 0 {
					ftsResults = append(ftsResults, ScoredFragment{
						Content: c.Content,
						Source:  c.ID,
						Score:   score,
					})
				}
			}
		}
	}

	var vecResults []ScoredFragment
	if e.embedder != nil && vecStore != nil { //nolint:nestif
		qEmbF32 := e.embedder.Embed(query)
		vecIter, err := vecStore.Scan(ctx, scope)
		if err == nil {
			defer vecIter.Close()
			for vecIter.Next() {
				var c struct {
					ID        string    `json:"id"`
					Content   string    `json:"content"`
					Embedding []float64 `json:"embedding"`
				}
				if err := json.Unmarshal(vecIter.Value(), &c); err == nil {
					if len(qEmbF32) > 0 && len(c.Embedding) == len(qEmbF32) {
						var dot, n1, n2 float64
						for i := range qEmbF32 {
							v1 := float64(qEmbF32[i])
							v2 := c.Embedding[i]
							dot += v1 * v2
							n1 += v1 * v1
							n2 += v2 * v2
						}
						if n1 > 0 && n2 > 0 {
							vecResults = append(vecResults, ScoredFragment{
								Content: c.Content,
								Source:  c.ID,
								Score:   dot / (n1 * n2), // approx
							})
						}
					}
				}
			}
		}
	}

	results := map[string][]ScoredFragment{
		"bm25":   ftsResults,
		"vector": vecResults,
	}
	weights := map[string]float64{
		"bm25":   config.BM25Weight,
		"vector": config.VectorWeight,
	}

	fused := RRFFuse(config.RRFK, weights, results)

	// ColBERT 近似重排：取 RerankTopM 候选送入 Reranker，再截断到 FinalTopK。
	// Reranker 为 nil 时跳过（等价于 NilReranker），不改变 RRF 排序结果。
	if config.Reranker != nil && config.RerankTopM > 0 && len(fused) > 0 {
		candidates := fused
		if len(candidates) > config.RerankTopM {
			candidates = candidates[:config.RerankTopM]
		}
		fused = config.Reranker.Rerank(ctx, query, candidates)
	}

	if config.FinalTopK > 0 && len(fused) > config.FinalTopK {
		fused = fused[:config.FinalTopK]
	}

	return fused, nil
}

func bm25Score(doc string, query string) float64 {
	docTerms := strings.Fields(strings.ToLower(doc))
	queryTerms := strings.Fields(strings.ToLower(query))
	if len(docTerms) == 0 || len(queryTerms) == 0 {
		return 0
	}

	tf := make(map[string]float64)
	for _, t := range docTerms {
		tf[t]++
	}

	k1 := 1.2
	b := 0.75
	avgdl := 100.0 // MVP approximate average document length

	score := 0.0
	for _, q := range queryTerms {
		f, ok := tf[q]
		if !ok {
			continue
		}
		// Approximate IDF without global corpus stats
		idf := 1.5
		score += idf * (f * (k1 + 1)) / (f + k1*(1-b+b*(float64(len(docTerms))/avgdl)))
	}
	return score
}

// HybridRetriever 共享引擎 — BM25 + Dense Vector + Graph Traversal 三路融合。
// M5 和 M10 共享底层 RRF+Rerank 引擎，检索范围和配置参数各自独立。
// 架构文档: docs/arch/05-Memory-System-深度选型.md §7.4,
//           docs/arch/10-Knowledge-RAG-深度选型.md §2.2

// RetrievalConfig 检索配置。
type RetrievalConfig struct {
	BM25Weight   float64 // M5:0.3, M10:0.3
	VectorWeight float64 // M5:0.6, M10:0.6
	GraphWeight  float64 // M5:0.1, M10:0.1
	RRFK         int     // 60
	OversampleN  int     // M5:3, M10:3
	RerankTopM   int     // M5:30, M10:50
	FinalTopK    int     // M5:10, M10:5
	// Reranker 后处理重排器。nil 或 NilReranker 跳过重排（默认行为）。
	// ApproximateColBERTReranker 可在 NewHybridSearchEngine 中注入。
	Reranker Reranker
}

// ScoredFragment 检索结果片段。
type ScoredFragment struct {
	Content  string
	Score    float64
	Source   string
	Metadata map[string]string
}

// HybridResult 三路召回原始结果。
type HybridResult struct {
	BM25Results  []ScoredFragment
	DenseResults []ScoredFragment
	GraphResults []ScoredFragment
}

// RRF Fuse 倒数排名融合。
// 公式: weight / (k + rank + 1), k=60.
// 三路累加后降序排列。
func RRFFuse(k int, weights map[string]float64, results map[string][]ScoredFragment) []ScoredFragment {
	scores := make(map[string]float64)
	for source, w := range weights {
		for rank, r := range results[source] {
			scores[r.Content] += w / float64(k+rank+1)
		}
	}

	var fused []ScoredFragment //nolint:prealloc
	for content, score := range scores {
		fused = append(fused, ScoredFragment{Content: content, Score: score})
	}

	// 按分数降序排序
	sort.Slice(fused, func(i, j int) bool {
		return fused[i].Score > fused[j].Score
	})

	return fused
}
