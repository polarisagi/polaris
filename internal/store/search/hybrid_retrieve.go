package search

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strings"

	"github.com/polarisagi/polaris/internal/store"

	"github.com/polarisagi/polaris/pkg/types"
)

// HybridSearchEngine 是 StorageRouter 之上的召回引擎（M10 Tier-0 路径，<8GB VPS）。
//
// 架构文档: docs/arch/05-Memory-System-深度选型.md §7.4,
//
//	docs/arch/10-Knowledge-RAG-深度选型.md §2.2
//
// 职责边界（GD-13-002 收敛终态）：本类型只负责**召回**（BM25 全表打分 +
// 向量线性扫描）与语料统计维护，**不做融合**。RRF 融合、ExplainBits 归因、
// Rerank、TopK 截断统一走 HybridSearch（见 hybrid_retriever.go）。
//
// 收敛前这里有一套独立的 Search + RRFFuse + 私有 ScoredFragment/RetrievalConfig，
// 与 memory/retrieval、knowledge 的两套实现并列构成**三套**融合逻辑——
// 而 GD-13-002 当初只识别出前两套。Tier-0（<8GB）恰恰走的是本条路径，
// 意味着最受资源约束的部署反而用着未被收敛、未被测试覆盖的那份融合代码。
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
// FlushTo 均已正确实现但从未被生产代码调用——重启后统计从零开始，
// FlushTo 也从未被任何后台 worker 触发过）。
func (e *HybridSearchEngine) Stats() *CorpusStats {
	return e.stats
}

// EmbedQuery 计算查询向量；未注入 embedder（纯 FTS 降级）时返回 nil。
// 暴露它而非让调用方持有 embedder：向量召回与查询编码必须用同一个模型实例，
// 分开持有会在 Tier 切换时出现"用 A 模型编码、拿 B 模型的向量库比对"。
func (e *HybridSearchEngine) EmbedQuery(query string) []float32 {
	if e.embedder == nil || query == "" {
		return nil
	}
	return e.embedder.Embed(query)
}

func (e *HybridSearchEngine) AddDocument(ctx context.Context, id, content string) error {
	if e.stats != nil {
		terms := strings.Fields(strings.ToLower(content))
		e.stats.AddDoc(terms)
	}
	return nil
}

// routerDocumentSource 把 HybridSearchEngine 的两路召回适配到统一融合管线
// （search.DocumentSource）。scope 是 KV 前缀（如 "chunk:"）。
type routerDocumentSource struct {
	engine       *HybridSearchEngine
	scope        []byte
	vecScanLimit int
}

var _ DocumentSource = (*routerDocumentSource)(nil)

// NewRouterDocumentSource 构造 StorageRouter 路径的 DocumentSource。
// vecScanLimit <= 0 时取 500（Tier-0 向量全表扫描安全上限）。
func (e *HybridSearchEngine) NewRouterDocumentSource(scope []byte, vecScanLimit int) DocumentSource {
	if vecScanLimit <= 0 {
		vecScanLimit = 500
	}
	return &routerDocumentSource{engine: e, scope: scope, vecScanLimit: vecScanLimit}
}

// SearchBM25 全表 BM25 打分召回（Tier-0 无倒排索引，只能扫描 + 逐条打分）。
func (s *routerDocumentSource) SearchBM25(ctx context.Context, query string, _ int) ([]types.ScoredFragment, error) {
	ftsStore := s.engine.router.Route(ctx, &store.StorageRequest{
		DataType:   "knowledge",
		AccessMode: "adhoc_query",
	})
	if ftsStore == nil {
		return nil, nil
	}
	iter, err := ftsStore.Scan(ctx, s.scope)
	if err != nil {
		// 主路扫描失败向上传播：BM25 是本路径唯一的关键词召回，
		// 静默返回空会让检索"看起来成功但什么都没召回"。
		return nil, err //nolint:wrapcheck // 由 HybridSearch 调用方按领域语义包装
	}
	defer iter.Close()

	var out []types.ScoredFragment
	for iter.Next() {
		var c struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(iter.Value(), &c); err != nil {
			slog.WarnContext(ctx, "hybrid_retrieve: bm25 doc unmarshal failed, skipping",
				"err", err, "doc_id", string(iter.Key()))
			continue
		}
		if score := bm25Score(c.Content, query, s.engine.stats); score > 0 {
			out = append(out, types.ScoredFragment{
				Content:      c.Content,
				Source:       c.ID,
				Score:        score,
				EvidenceType: types.EvidenceFTSKeyword,
			})
		}
	}
	return out, nil
}

// SearchVector Tier-0 向量召回：线性扫描 + Go 余弦，受 vecScanLimit 硬上限保护。
func (s *routerDocumentSource) SearchVector(ctx context.Context, embedding []float32, _ int) ([]types.ScoredFragment, error) {
	if s.engine.embedder == nil || len(embedding) == 0 {
		return nil, nil
	}
	vecStore := s.engine.router.Route(ctx, &store.StorageRequest{
		DataType:   "knowledge",
		AccessMode: "knn_read",
	})
	if vecStore == nil {
		return nil, nil
	}
	iter, err := vecStore.Scan(ctx, s.scope)
	if err != nil {
		// 向量路失败只降级（融合层会记录 Warn 并继续用 BM25 结果）。
		return nil, err //nolint:wrapcheck // 融合层按辅路语义降级处理
	}
	defer iter.Close()

	var out []types.ScoredFragment
	scanned := 0
	for iter.Next() {
		if scanned >= s.vecScanLimit {
			slog.WarnContext(ctx, "hybrid_retrieve: vector scan limit reached, truncating",
				"limit", s.vecScanLimit, "scope", string(s.scope))
			break
		}
		scanned++

		var c struct {
			ID        string    `json:"id"`
			Content   string    `json:"content"`
			Embedding []float64 `json:"embedding"`
		}
		if err := json.Unmarshal(iter.Value(), &c); err != nil {
			slog.WarnContext(ctx, "hybrid_retrieve: vector doc unmarshal failed, skipping",
				"err", err, "doc_id", string(iter.Key()))
			continue
		}
		if sim, ok := cosineF32F64(embedding, c.Embedding); ok {
			et := types.EvidenceWeakSemantic
			if sim >= 0.85 {
				et = types.EvidenceHighVector
			}
			out = append(out, types.ScoredFragment{
				Content:      c.Content,
				Source:       c.ID,
				Score:        sim,
				EvidenceType: et,
			})
		}
	}
	return out, nil
}

// SearchGraph StorageRouter 路径无图遍历能力，按 DocumentSource 契约返回空而非错误。
func (s *routerDocumentSource) SearchGraph(context.Context, string, int) ([]types.ScoredFragment, error) {
	return nil, nil
}

// cosineF32F64 计算 float32 查询向量与 float64 文档向量的余弦相似度。
// 维度不一致或任一向量为零向量时返回 ok=false（该文档跳过，不计入召回）。
func cosineF32F64(q []float32, d []float64) (float64, bool) {
	if len(q) == 0 || len(d) != len(q) {
		return 0, false
	}
	var dot, n1, n2 float64
	for i := range q {
		v1 := float64(q[i])
		v2 := d[i]
		dot += v1 * v2
		n1 += v1 * v1
		n2 += v2 * v2
	}
	if n1 <= 0 || n2 <= 0 {
		return 0, false
	}
	return dot / math.Sqrt(n1*n2), true
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
