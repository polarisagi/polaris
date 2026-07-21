package knowledge

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// VectorEmbedder 向量嵌入接口（consumer-side，防止包循环）。
// Tier 0 可传 nil，降级为纯 FTS5；Tier 1+ 注入 substrate.EmbeddingBatcher 实现。
type VectorEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// CognitiveSearcher M10 知识专属的认知搜索引擎（Consumer-side）。
// 用于 HybridRetrieverImpl.Search 并发支路调用。
type CognitiveSearcher interface {
	VecKNN(query []float32, k int) ([]types.CognitiveSearchResult, error)
	FTSSearch(query string, k int) ([]types.CognitiveSearchResult, error)
}

// HybridRetrieverImpl 实现 HybridRetriever。
// 检索策略:
//   - Tier 0 (embedder=nil): FTS5 BM25 单路，按 rank 排序。
//   - Tier 1+ (embedder 非 nil): FTS5 + Dense Vector 双路 RRF 融合。
type HybridRetrieverImpl struct {
	db                 protocol.SQLQuerier
	embedder           VectorEmbedder                 // optional，nil = FTS5 only
	cognitive          CognitiveSearcher              // optional，Tier 1+ SurrealDB HNSW
	graph              *graphrag.GraphTraverser       // optional，Tier 0+ 图遍历（M10 §2.6）
	reranker           protocol.Reranker              // optional，Cross-encoder reranking
	vecScanLimit       int                            // Tier0VectorScanLimit
	boundarySerializer *taint.TaintBoundarySerializer // 可选；nil 时不校验 taint_hmac（inv_M11_02）
}

var _ protocol.HybridRetriever = (*HybridRetrieverImpl)(nil)

func (hr *HybridRetrieverImpl) SetReranker(r protocol.Reranker) {
	hr.reranker = r
}

// SetBoundarySerializer 注入跨边界 HMAC 校验器（inv_M11_02）。与 SetReranker
// 同为启动期热注入 setter：boot_knowledge.go 组合根按 Tier 装配检索栈时，
// TaintBoundarySerializer 由 sb.Vault 派生，构造顺序上晚于 retriever 本身。
func (hr *HybridRetrieverImpl) SetBoundarySerializer(ser *taint.TaintBoundarySerializer) {
	hr.boundarySerializer = ser
}

// 2026-07-14（ADR-0051）：NewHybridRetriever/NewHybridRetrieverWithEmbedder/
// NewHybridRetrieverWithGraph 删除——boot_knowledge.go 生产两条检索栈装配路径
// （SurrealStore≠nil 用 WithCognitive；SurrealStore==nil 用
// NewDefaultHybridRetriever/StorageRouter）都不经过本类型的这 3 个平行构造函数，
// hr.graph 字段在生产中永远为 nil（无任何调用点为其注入值），graph 检索分支
// 结构上不可达。embedder/cognitive/graph 均可传 nil 走对应降级路径，
// NewHybridRetrieverWithCognitive 是本类型唯一生产构造入口。

// NewHybridRetrieverWithCognitive 创建含 SurrealDB HNSW 路径的全功能 HybridRetriever（Tier 1+）。
func NewHybridRetrieverWithCognitive(db protocol.SQLQuerier, embedder VectorEmbedder, cognitive CognitiveSearcher, vecScanLimit int) *HybridRetrieverImpl {
	if vecScanLimit <= 0 {
		vecScanLimit = 500
	}
	return &HybridRetrieverImpl{db: db, embedder: embedder, cognitive: cognitive, vecScanLimit: vecScanLimit}
}

// Search 执行混合检索。
func (hr *HybridRetrieverImpl) Search(ctx context.Context, query string, scope types.SearchScope, config types.RetrievalConfig) ([]types.ScoredFragment, error) {
	tracer := trace.NewTracer()
	span, ctx := tracer.StartSpan(ctx, trace.SpanMemoryOp, "Knowledge.Search")
	defer tracer.EndSpan(span)

	if query == "" {
		return nil, nil
	}
	topK := config.FinalTopK
	if topK <= 0 {
		topK = 10
	}

	var wg sync.WaitGroup
	var ftsErr error
	var ftsResults, vecResults, graphResults []Chunk

	wg.Add(1)
	concurrent.SafeGo(ctx, "knowledge.retriever.search_fts", func(ctx context.Context) {
		defer wg.Done()
		res, err := hr.searchFTS(ctx, query, topK*3)
		if err != nil {
			ftsErr = err
			return
		}
		ftsResults = res
	})

	if hr.embedder != nil {
		wg.Add(1)
		concurrent.SafeGo(ctx, "knowledge.retriever.search_vector", func(ctx context.Context) {
			defer wg.Done()
			vr, err := hr.searchVector(ctx, query, topK*3)
			if err == nil {
				vecResults = vr
			}
		})
	}

	if hr.graph != nil {
		wg.Add(1)
		concurrent.SafeGo(ctx, "knowledge.retriever.search_graph", func(ctx context.Context) {
			defer wg.Done()
			gr, err := hr.graph.TraverseChunks(ctx, query, topK*3)
			if err == nil {
				graphResults = gr
			}
		})
	}

	wg.Wait()

	if ftsErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "fts search failed", ftsErr)
	}

	// RRF 三路融合（有 vector 或 graph 时才融合）
	var finalResults []Chunk
	if len(vecResults) > 0 || len(graphResults) > 0 {
		finalResults = rrfThreeWay(ftsResults, vecResults, graphResults, topK*2)
	} else {
		finalResults = ftsResults
	}

	if hr.reranker != nil && len(finalResults) > 0 {
		finalResults = hr.applyReranker(ctx, query, finalResults)
	}

	if len(finalResults) > topK {
		finalResults = finalResults[:topK]
	}

	bitsByID := explainBitsByChunkID(ftsResults, vecResults, graphResults)
	return toScoredFragments(ctx, finalResults, bitsByID), nil
}

// explainBitsByChunkID 记录每个 chunk ID 命中的检索路径（GR-1-003/Batch8 ExplainBits 归因修复）。
// rrfThreeWay 融合后原始来源信息即丢失，必须在融合前的三路原始结果上标记；这里用 ID
// 反查而非改造 Chunk 结构体，因为 ExplainBits 是检索期概念，不属于 graphrag.Chunk 这个
// 跨包共享的文档域类型（避免污染无关调用方）。从 Search 中拆出以控制圈复杂度（R7 gocyclo）。
func explainBitsByChunkID(ftsResults, vecResults, graphResults []Chunk) map[string]uint8 {
	bitsByID := make(map[string]uint8, len(ftsResults)+len(vecResults)+len(graphResults))
	for _, c := range ftsResults {
		bitsByID[c.ID] |= types.BitBM25
	}
	for _, c := range vecResults {
		bitsByID[c.ID] |= types.BitVector
	}
	for _, c := range graphResults {
		bitsByID[c.ID] |= types.BitGraph
	}
	return bitsByID
}

// toScoredFragments 将最终 Chunk 结果转换为 ScoredFragment，并上报每条结果的 ExplainBits 归因指标。
func toScoredFragments(ctx context.Context, finalResults []Chunk, bitsByID map[string]uint8) []types.ScoredFragment {
	scored := make([]types.ScoredFragment, len(finalResults))
	for i, c := range finalResults {
		bits := bitsByID[c.ID]
		recordExplainBits(ctx, bits)
		scored[i] = types.ScoredFragment{
			Content:     c.Content,
			Source:      c.SourceURI,
			TaintLevel:  types.TaintLevel(c.TaintLevel),
			ExplainBits: bits,
		}
	}
	return scored
}

func (hr *HybridRetrieverImpl) applyReranker(ctx context.Context, queryText string, results []Chunk) []Chunk {
	docs := make([]types.CognitiveSearchResult, len(results))
	chunkMap := make(map[string]Chunk)
	for i, c := range results {
		docs[i] = types.CognitiveSearchResult{
			ID:      c.ID,
			Score:   0,
			Content: c.Content,
		}
		chunkMap[c.ID] = c
	}
	rerankedDocs, err := hr.reranker.Rerank(ctx, queryText, docs)
	if err != nil {
		slog.Warn("knowledge: reranker failed, fallback to original order", "err", err)
		return results
	}
	finalResults := make([]Chunk, 0, len(rerankedDocs))
	for _, rd := range rerankedDocs {
		if c, ok := chunkMap[rd.ID]; ok {
			finalResults = append(finalResults, c)
		}
	}
	return finalResults
}

// searchFTS 使用 FTS5 BM25 检索，返回 limit 条结果。
func (hr *HybridRetrieverImpl) searchFTS(ctx context.Context, queryText string, limit int) ([]Chunk, error) {
	sqlQuery := `
		SELECT rc.id, rc.doc_id, rc.content, rc.taint_level, rc.taint_source, rc.taint_hmac
		FROM rag_chunks rc
		WHERE rc.rowid IN (
			SELECT rowid FROM rag_chunks_fts
			WHERE rag_chunks_fts MATCH ?
			ORDER BY rank
			LIMIT ?
		) AND rc.deleted_at IS NULL
	`
	rows, err := hr.db.QueryContext(ctx, sqlQuery, queryText, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hybrid_retriever: fts search failed", err)
	}
	defer rows.Close()

	var results []Chunk
	for rows.Next() {
		var chunk Chunk
		var taintSource, taintHMAC sql.NullString
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource, &taintHMAC); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to scan fts row", err)
		}
		if taintSource.Valid {
			chunk.TaintSource = taintSource.String
		}
		chunk.TaintLevel = verifyChunkTaint(hr.boundarySerializer, chunk.ID, chunk.Content, chunk.TaintLevel, chunk.TaintSource, taintHMAC.String)
		results = append(results, chunk)
	}
	return results, rows.Err()
}

// searchVector 从 rag_chunks 读取已存储的 embedding，计算余弦相似度，返回 top-limit 条。
// 仅对存储了 embedding 的 chunk 生效；无 embedding 的 chunk 跳过（向量路径幂等）。
func (hr *HybridRetrieverImpl) searchVector(ctx context.Context, queryText string, limit int) ([]Chunk, error) { //nolint:gocyclo,nestif
	queryEmbed, err := hr.embedder.Embed(ctx, queryText)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to embed query text", err)
	}
	if len(queryEmbed) == 0 {
		return nil, nil
	}

	// Tier 1+: SurrealDB HNSW (O(log N))
	if hr.cognitive != nil {
		if hits, vecErr := hr.cognitive.VecKNN(queryEmbed, limit); vecErr == nil {
			return hr.fetchCognitiveHits(ctx, hits)
		} else {
			// BUG-4 修复：VecKNN 错误不再静默吞噬，保证 HE-Rule §1 可观测性
			slog.Warn("knowledge: SurrealDB VecKNN failed, degrading to linear scan", "err", vecErr)
		}
	}

	return hr.searchVectorFallback(ctx, queryEmbed, limit)
}

// fetchCognitiveHits 从 cognitive search hits (含有 ID 和 Score) 还原完整的 chunk。
// 因为 SurrealDB 层只返回了 ID，我们需要再回到 StorageRouter 查出 Document 和 Chunk 数据。
func (hr *HybridRetrieverImpl) fetchCognitiveHits(ctx context.Context, hits []types.CognitiveSearchResult) ([]Chunk, error) {
	var results []Chunk
	for _, h := range hits {
		var chunk Chunk
		var taintSource, taintHMAC sql.NullString
		// BUG-3 修复：SELECT 补全全部 lineage 字段，确保 inv_M10_03 溯源完整性不变量
		err := hr.db.QueryRowContext(ctx, `
			SELECT id, doc_id, content, taint_level, taint_source, taint_hmac,
			       source_uri, doc_version, chunk_seq, content_hash, embed_model_version
			FROM rag_chunks WHERE id = ? AND deleted_at IS NULL`, h.ID).
			Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource, &taintHMAC,
				&chunk.SourceURI, &chunk.DocVersion, &chunk.ChunkSeq, &chunk.ContentHash, &chunk.EmbedModelVersion)
		if err == nil {
			if taintSource.Valid {
				chunk.TaintSource = taintSource.String
			}
			chunk.TaintLevel = verifyChunkTaint(hr.boundarySerializer, chunk.ID, chunk.Content, chunk.TaintLevel, chunk.TaintSource, taintHMAC.String)
			results = append(results, chunk)
		}
	}
	return results, nil
}

// searchVectorFallback 线性扫描降级
func (hr *HybridRetrieverImpl) searchVectorFallback(ctx context.Context, queryEmbed []float32, limit int) ([]Chunk, error) {

	// Tier 0 降级：读取所有有 embedding 的 chunk（线性扫描）
	rows, err := hr.db.QueryContext(ctx, `
		SELECT id, doc_id, content, taint_level, taint_source, taint_hmac, embedding
		FROM rag_chunks
		WHERE embedding IS NOT NULL AND embedding != '' AND deleted_at IS NULL
		LIMIT ?
	`, hr.vecScanLimit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hybrid_retriever: vector scan failed", err)
	}
	defer rows.Close()

	type scored struct {
		chunk Chunk
		score float64
	}
	var scored_ []scored

	for rows.Next() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		var chunk Chunk
		var taintSource, taintHMAC sql.NullString
		var embJSON sql.NullString
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource, &taintHMAC, &embJSON); err != nil {
			continue
		}
		if taintSource.Valid {
			chunk.TaintSource = taintSource.String
		}
		chunk.TaintLevel = verifyChunkTaint(hr.boundarySerializer, chunk.ID, chunk.Content, chunk.TaintLevel, chunk.TaintSource, taintHMAC.String)
		if !embJSON.Valid || embJSON.String == "" {
			continue
		}
		chunkEmbed, parseErr := parseEmbedding(embJSON.String)
		if parseErr != nil || len(chunkEmbed) != len(queryEmbed) {
			continue
		}
		sim := cosine(queryEmbed, chunkEmbed)
		scored_ = append(scored_, struct {
			chunk Chunk
			score float64
		}{chunk, sim})
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to scan rows for fallback vector search", err)
	}

	sort.Slice(scored_, func(i, j int) bool { return scored_[i].score > scored_[j].score })
	if len(scored_) > limit {
		scored_ = scored_[:limit]
	}
	results := make([]Chunk, len(scored_))
	for i, s := range scored_ {
		results[i] = s.chunk
	}
	return results, nil
}

// cosine 计算两个向量的余弦相似度（[0,1]）。
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// 向量降级路径的 JSON/float 轻量解析（parseEmbedding/parseFloat/pf*）与三路 RRF
// 融合（rrfThreeWay）见 retriever_parsing.go（R7 拆分）。
