package search

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/store"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// HybridSearchEngine 提供统一接口: Search(ctx, query, scope, config) → []ScoredFragment
type HybridSearchEngine struct {
	router   *store.StorageRouter
	embedder Embedder
	stats    *CorpusStats
}

func NewHybridSearchEngine(router *store.StorageRouter, embedder Embedder) *HybridSearchEngine {
	return &HybridSearchEngine{
		router:   router,
		embedder: embedder,
		stats:    NewCorpusStats(),
	}
}

// Stats 暴露内部 CorpusStats，供调用方在启动时 RestoreStatsFromDB 恢复历史统计、
// 并周期性 FlushTo 持久化增量（2026-07-04 审计补齐，任务18：此前 RestoreStatsFromDB/
// FlushTo 均已正确实现但从未被生产代码调用，是纯死代码——重启后统计从零开始，
// FlushTo 也从未被任何后台 worker 触发过）。
func (e *HybridSearchEngine) Stats() *CorpusStats {
	return e.stats
}

func (e *HybridSearchEngine) AddDocument(ctx context.Context, id, content string) error {
	if e.stats != nil {
		terms := strings.Fields(strings.ToLower(content))
		e.stats.AddDoc(terms)
	}
	return nil
}

func (e *HybridSearchEngine) Search(ctx context.Context, query string, scope []byte, config RetrievalConfig) ([]ScoredFragment, error) { //nolint:gocyclo,nestif
	if query == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "empty query")
	}

	ftsStore := e.router.Route(ctx, &store.StorageRequest{
		DataType:   "knowledge",
		AccessMode: "adhoc_query",
	})
	vecStore := e.router.Route(ctx, &store.StorageRequest{
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
				score := bm25Score(c.Content, query, e.stats)
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

			vecScanLimit := config.Tier0VectorScanLimit
			if vecScanLimit <= 0 {
				vecScanLimit = 500
			}
			vecScanned := 0

			for vecIter.Next() {
				if vecScanned >= vecScanLimit {
					slog.Warn("hybrid_retrieve: vector scan limit reached, truncating",
						"limit", vecScanLimit,
						"scope", string(scope),
					)
					break
				}
				vecScanned++

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
								Score:   dot / math.Sqrt(n1*n2),
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

	// sourceBits 供 RRFFuse 标记每条结果由哪几路召回贡献（GR-1-003/Batch8 ExplainBits 归因）。
	sourceBits := map[string]uint8{
		"bm25":   types.BitBM25,
		"vector": types.BitVector,
	}
	fused := RRFFuse(config.RRFK, weights, results, sourceBits)

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

func bm25Score(doc string, query string, stats *CorpusStats) float64 {
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
	var avgdl float64
	if stats != nil {
		avgdl = stats.AvgDocLen()
	} else {
		avgdl = 100.0 // MVP approximate average document length
	}

	score := 0.0
	for _, q := range queryTerms {
		f, ok := tf[q]
		if !ok {
			continue
		}
		var idf float64
		if stats != nil {
			idf = stats.IDF(q)
		} else {
			idf = 1.5
		}
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

	Tier0VectorScanLimit int // 向量全表扫描安全上限
}

// ScoredFragment 检索结果片段。
type ScoredFragment struct {
	Content  string
	Score    float64
	Source   string
	Metadata map[string]string
	// ExplainBits 检索路径位图（线上排障用，不进 Prompt）。位定义与
	// pkg/types.ScoredFragment.ExplainBits 一致，由 RRFFuse 的 sourceBits 参数填充
	// （GR-1-003/Batch8 ExplainBits 归因修复）。
	ExplainBits uint8
}

// HybridResult 三路召回原始结果。
type HybridResult struct {
	BM25Results  []ScoredFragment
	DenseResults []ScoredFragment
	GraphResults []ScoredFragment
}

// RRFFuse 倒数排名融合。
// 公式: weight / (k + rank + 1), k=60。三路累加后降序排列。
// key = Source（优先）或 Content（Source 为空时兜底），保留首次出现的完整字段（Source/Metadata）。
// sourceBits 将 results 的 key（如 "bm25"/"vector"/"graph"）映射到 ExplainBits 位掩码，
// 用于标记每条融合结果具体由哪几路召回贡献（GR-1-003/Batch8 ExplainBits 归因修复）。
// 三路结果在此处融合前是分开存放的（HybridResult），融合后归因信息会丢失，必须在这里
// （融合的同时）记录，融合完成后无法反推。sourceBits 传 nil 时跳过归因（ExplainBits 保持
// 零值，兼容不需要归因的调用方）。
func RRFFuse(k int, weights map[string]float64, results map[string][]ScoredFragment, sourceBits map[string]uint8) []ScoredFragment {
	scores := make(map[string]float64)
	frags := make(map[string]ScoredFragment) // key → 首次出现的完整 fragment（保留 Source/Metadata）
	bits := make(map[string]uint8)           // key → 命中路径位图（only-up 累加，见 HE-7）

	for source, w := range weights {
		for rank, r := range results[source] {
			// Source 为空时退化到 Content（兜底），避免不同来源相同内容互相覆盖分数
			key := r.Source
			if key == "" {
				key = r.Content
			}
			scores[key] += w / float64(k+rank+1)
			if _, seen := frags[key]; !seen {
				frags[key] = r // 首次出现，保留 Source/Metadata 等原始字段
			}
			if sourceBits != nil {
				bits[key] |= sourceBits[source]
			}
		}
	}

	fused := make([]ScoredFragment, 0, len(scores))
	for key, score := range scores {
		frag := frags[key]
		frag.Score = score
		frag.ExplainBits |= bits[key]
		fused = append(fused, frag)
	}

	// 按分数降序排序
	sort.Slice(fused, func(i, j int) bool {
		return fused[i].Score > fused[j].Score
	})

	return fused
}
