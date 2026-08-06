package knowledge

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
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

// SetGraphTraverser 注入 M10 §2.6 LocalSearch 图遍历器，激活融合管线的第三路
// （2026-08-06 接线）。nil 时该路返回空结果，退化为 BM25+Vector 两路。
func (hr *HybridRetrieverImpl) SetGraphTraverser(g *graphrag.GraphTraverser) {
	hr.graph = g
}

// SetBoundarySerializer 注入跨边界 HMAC 校验器（inv_M11_02）。与 SetReranker
// 同为启动期热注入 setter：boot_knowledge.go 组合根按 Tier 装配检索栈时，
// TaintBoundarySerializer 由 sb.Vault 派生，构造顺序上晚于 retriever 本身。
func (hr *HybridRetrieverImpl) SetBoundarySerializer(ser *taint.TaintBoundarySerializer) {
	hr.boundarySerializer = ser
}

// 2026-07-14（ADR-0062）：NewHybridRetriever/NewHybridRetrieverWithEmbedder/
// NewHybridRetrieverWithGraph 删除——boot_knowledge.go 生产两条检索栈装配路径
// （SurrealStore≠nil 用 WithCognitive；SurrealStore==nil 用
// NewDefaultHybridRetriever/StorageRouter）都不经过本类型的这 3 个平行构造函数，
// hr.graph 字段在生产中永远为 nil（无任何调用点为其注入值），graph 检索分支
// 结构上不可达。embedder/cognitive/graph 均可传 nil 走对应降级路径，
// NewHybridRetrieverWithCognitive 是本类型唯一生产构造入口。
//
// 2026-08-06 终态处置（取代 V-5 注记）：graph 字段改为**接线**而非删除。
//
// V-5（2026-07-23）当时的判断是"无 WIRE 决议 → 按 deadcode 纪律理应删除，
// 但级联影响面大，暂留待专项 ADR"。本轮重新评估后选择接线，理由：
//   - graphrag.GraphTraverser 是一份完整可用的实现（318 行 BFS + 双时态
//     AsOf 过滤 + 深度衰减评分），缺的只是一个构造入口——删掉是丢能力，
//     不是清死代码；
//   - GD-13-002 收敛后 knowledge 走统一融合管线，M10 §2.2 本就为 Graph 留了
//     第三路权重（0.1）。不接线的话这一路恒为空，等于权重配置在描述一个
//     不存在的东西；
//   - V-5 提到的级联障碍（rrfThreeWay/explainBitsByChunkID 签名）已随
//     GD-13-002 收敛一并消失——那两个函数都已删除。
//
// 接线点：graphrag.NewGraphTraverser（同轮补回）+ SetGraphTraverser，
// 由 cmd/polaris/boot_knowledge.go 在 SurrealDB 路径上注入。

// NewHybridRetrieverWithCognitive 创建含 SurrealDB HNSW 路径的全功能 HybridRetriever（Tier 1+）。
func NewHybridRetrieverWithCognitive(db protocol.SQLQuerier, embedder VectorEmbedder, cognitive CognitiveSearcher, vecScanLimit int) *HybridRetrieverImpl {
	if vecScanLimit <= 0 {
		vecScanLimit = 500
	}
	return &HybridRetrieverImpl{db: db, embedder: embedder, cognitive: cognitive, vecScanLimit: vecScanLimit}
}

// Search 执行混合检索。
//
//nolint:gocyclo,nestif
func (hr *HybridRetrieverImpl) Search(ctx context.Context, query string, scope types.SearchScope, config types.RetrievalConfig) ([]types.ScoredFragment, error) {
	tracer := trace.NewTracer()
	span, ctx := tracer.StartSpan(ctx, trace.SpanMemoryOp, "Knowledge.Search")
	defer tracer.EndSpan(span)

	if query == "" {
		return nil, nil
	}
	topK := config.FinalTopK
	if topK <= 0 {
		topK = defaultKnowledgeTopK
	}

	var isMacro bool
	if len(strings.Fields(query)) <= 4 {
		var ec int
		if err := hr.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM semantic_entities WHERE name = ? AND entity_type != 'Community'", query).Scan(&ec); err != nil {
			slog.Warn("knowledge_retriever: entity count query failed, degrading to macro/Community search", "query", query, "err", err)
			metrics.RecordKnowledgeReadFailure(ctx, "macro_query_entity_count")
		}
		isMacro = (ec == 0)
	}

	var queryEmbed []float32
	if hr.embedder != nil {
		if emb, err := hr.embedder.Embed(ctx, query); err == nil {
			queryEmbed = emb
		}
	}

	src := &knowledgeDocumentSource{
		hr:      hr,
		isMacro: isMacro,
	}

	var searchReranker search.Reranker
	if hr.reranker != nil {
		searchReranker = (*rerankerAdapter)(hr)
	}

	// 融合前先截到 topK*2（重构前 rrfThreeWay(..., topK*2)），置顶宏观摘要后
	// 再统一截到 topK——顺序不能颠倒，否则宏观摘要会挤掉正常召回的名额。
	merged, err := search.HybridSearch(ctx, src, query, queryEmbed, search.HybridSearchConfig{
		BM25Weight:   knowledgeBM25Weight,
		VectorWeight: knowledgeVectorWeight,
		GraphWeight:  knowledgeGraphWeight,
		RRFk:         knowledgeRRFk,
		TopK:         topK * 2,
		RecallWidth:  topK * 3,
		EnableRerank: hr.reranker != nil,
		Reranker:     searchReranker,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hybrid_retriever: search failed", err)
	}

	// 宏观查询：Community 摘要置顶（重构前 append(macroResults, finalResults...)）。
	if macro := src.macroResults(); len(macro) > 0 {
		merged = append(macro, merged...)
	}
	if len(merged) > topK {
		merged = merged[:topK]
	}

	// Source 语义还原：融合期用 chunk ID 做去重键，对外必须输出 SourceURI
	// （调用方据此做引用溯源，重构前 toScoredFragments 即如此）。
	for i := range merged {
		merged[i].Source = src.resolveURI(merged[i].Source)
		recordExplainBits(ctx, merged[i].ExplainBits)
	}

	return merged, nil
}

type rerankerAdapter HybridRetrieverImpl

func (r *rerankerAdapter) Rerank(ctx context.Context, query string, docs []types.ScoredFragment) ([]types.ScoredFragment, error) {
	hr := (*HybridRetrieverImpl)(r)
	if hr.reranker == nil {
		return docs, nil
	}

	// Convert types.ScoredFragment to types.CognitiveSearchResult
	cDocs := make([]types.CognitiveSearchResult, len(docs))
	for i, d := range docs {
		cDocs[i] = types.CognitiveSearchResult{
			ID:      d.Source,
			Score:   d.Score,
			Content: d.Content,
		}
	}

	rerankedDocs, err := hr.reranker.Rerank(ctx, query, cDocs)
	if err != nil {
		slog.Warn("knowledge: reranker failed, fallback to original order", "err", err)
		return docs, nil
	}

	// Create a map to quickly recover original fields
	docMap := make(map[string]types.ScoredFragment, len(docs))
	for _, d := range docs {
		docMap[d.Source] = d
	}

	finalResults := make([]types.ScoredFragment, 0, len(rerankedDocs))
	for _, rd := range rerankedDocs {
		if orig, ok := docMap[rd.ID]; ok {
			finalResults = append(finalResults, orig)
		}
	}
	return finalResults, nil
}

// searchFTS 使用 FTS5 BM25 检索，返回 limit 条结果。
func (hr *HybridRetrieverImpl) searchFTS(ctx context.Context, queryText string, limit int) ([]Chunk, error) {
	sqlQuery := `
		SELECT rc.id, rc.doc_id, rc.content, rc.taint_level, rc.taint_source, rc.taint_hmac, rc.source_uri, rc.doc_version, rc.chunk_seq, rc.content_hash, rc.embed_model_version
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
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource, &taintHMAC, &chunk.SourceURI, &chunk.DocVersion, &chunk.ChunkSeq, &chunk.ContentHash, &chunk.EmbedModelVersion); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to scan fts row", err)
		}
		if taintSource.Valid {
			chunk.TaintSource = taintSource.String
		}
		chunk.TaintLevel = verifyChunkTaint(hr.boundarySerializer, chunk.ID, chunk.Content, chunk.TaintLevel, chunk.TaintSource, taintHMAC.String)
		results = append(results, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "hybrid_retriever: 遍历 fts 结果集失败", err)
	}
	return results, nil
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
		SELECT id, doc_id, content, taint_level, taint_source, taint_hmac, embedding, source_uri, doc_version, chunk_seq, content_hash, embed_model_version
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
			return nil, ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		default:
		}
		var chunk Chunk
		var taintSource, taintHMAC sql.NullString
		var embJSON sql.NullString
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource, &taintHMAC, &embJSON, &chunk.SourceURI, &chunk.DocVersion, &chunk.ChunkSeq, &chunk.ContentHash, &chunk.EmbedModelVersion); err != nil {
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
