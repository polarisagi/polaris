package knowledge

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// VectorEmbedder 向量嵌入接口（consumer-side，防止包循环）。
// Tier 0 可传 nil，降级为纯 FTS5；Tier 1+ 注入 substrate.EmbeddingBatcher 实现。
type VectorEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// CognitiveSearchResult 认知检索结果（consumer-side）。
type CognitiveSearchResult struct {
	ID    string
	Score float64
}

// CognitiveSearcher consumer-side 接口：SurrealDB FTS + HNSW 向量检索（Tier1+）。
type CognitiveSearcher interface {
	VecKNN(query []float32, k int) ([]CognitiveSearchResult, error)
	FTSSearch(query string, k int) ([]CognitiveSearchResult, error)
}

// HybridRetrieverImpl 实现 HybridRetriever。
// 检索策略:
//   - Tier 0 (embedder=nil): FTS5 BM25 单路，按 rank 排序。
//   - Tier 1+ (embedder 非 nil): FTS5 + Dense Vector 双路 RRF 融合。
type HybridRetrieverImpl struct {
	db        protocol.SQLQuerier
	embedder  VectorEmbedder           // optional，nil = FTS5 only
	cognitive CognitiveSearcher        // optional，Tier 1+ SurrealDB HNSW
	graph     *graphrag.GraphTraverser // optional，Tier 0+ 图遍历（M10 §2.6）
}

var _ HybridRetriever = (*HybridRetrieverImpl)(nil)

// NewHybridRetriever 创建 FTS5-only 检索器（Tier 0）。
func NewHybridRetriever(db protocol.SQLQuerier) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{db: db}
}

// NewHybridRetrieverWithEmbedder 创建含密集向量路径的检索器（Tier 1+）。
func NewHybridRetrieverWithEmbedder(db protocol.SQLQuerier, embedder VectorEmbedder) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{db: db, embedder: embedder}
}

// NewHybridRetrieverWithCognitive 创建含 SurrealDB HNSW 路径的全功能 HybridRetriever（Tier 1+）。
func NewHybridRetrieverWithCognitive(db protocol.SQLQuerier, embedder VectorEmbedder, cognitive CognitiveSearcher) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{db: db, embedder: embedder, cognitive: cognitive}
}

// NewHybridRetrieverWithGraph 创建含 GraphTraverser 的全功能 HybridRetriever。
func NewHybridRetrieverWithGraph(db protocol.SQLQuerier, embedder VectorEmbedder, cognitive CognitiveSearcher, graph *graphrag.GraphTraverser) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{db: db, embedder: embedder, cognitive: cognitive, graph: graph}
}

// Search 执行混合检索。
// TopK ≤ 0 时默认返回 10 条。
func (hr *HybridRetrieverImpl) Search(ctx context.Context, query *SearchQuery) ([]Chunk, error) {
	if query == nil || query.Text == "" {
		return nil, nil
	}
	topK := query.TopK
	if topK <= 0 {
		topK = 10
	}

	// FTS5 路径（始终执行）
	ftsResults, err := hr.searchFTS(ctx, query.Text, topK*3) // 宽召回 3×TopK
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "fts search failed", err)
	}

	// 向量路径（Tier 1+）
	var vecResults []Chunk
	if hr.embedder != nil {
		vr, vecErr := hr.searchVector(ctx, query.Text, topK*3)
		if vecErr == nil {
			vecResults = vr
		}
	}

	// 图遍历路径（Graph weight=0.1，M10 §2.2）
	var graphResults []Chunk
	if hr.graph != nil {
		gr, grErr := hr.graph.TraverseChunks(ctx, query.Text, topK*3)
		if grErr == nil {
			graphResults = gr
		}
	}

	// RRF 三路融合（有 vector 或 graph 时才融合）
	if len(vecResults) > 0 || len(graphResults) > 0 {
		return rrfThreeWay(ftsResults, vecResults, graphResults, topK), nil
	}

	if len(ftsResults) > topK {
		ftsResults = ftsResults[:topK]
	}
	return ftsResults, nil
}

// searchFTS 使用 FTS5 BM25 检索，返回 limit 条结果。
func (hr *HybridRetrieverImpl) searchFTS(ctx context.Context, queryText string, limit int) ([]Chunk, error) {
	sqlQuery := `
		SELECT rc.id, rc.doc_id, rc.content, rc.taint_level, rc.taint_source
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
		var taintSource sql.NullString
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to scan fts row", err)
		}
		if taintSource.Valid {
			chunk.TaintSource = taintSource.String
		}
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

func (hr *HybridRetrieverImpl) fetchCognitiveHits(ctx context.Context, hits []CognitiveSearchResult) ([]Chunk, error) {
	var results []Chunk
	for _, h := range hits {
		var chunk Chunk
		var taintSource sql.NullString
		// BUG-3 修复：SELECT 补全全部 lineage 字段，确保 inv_M10_03 溯源完整性不变量
		err := hr.db.QueryRowContext(ctx, `
			SELECT id, doc_id, content, taint_level, taint_source,
			       source_uri, doc_version, chunk_seq, content_hash, embed_model_version
			FROM rag_chunks WHERE id = ? AND deleted_at IS NULL`, h.ID).
			Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource,
				&chunk.SourceURI, &chunk.DocVersion, &chunk.ChunkSeq, &chunk.ContentHash, &chunk.EmbedModelVersion)
		if err == nil {
			if taintSource.Valid {
				chunk.TaintSource = taintSource.String
			}
			results = append(results, chunk)
		}
	}
	return results, nil
}

// searchVectorFallback 线性扫描降级
func (hr *HybridRetrieverImpl) searchVectorFallback(ctx context.Context, queryEmbed []float32, limit int) ([]Chunk, error) {

	// Tier 0 降级：读取所有有 embedding 的 chunk（线性扫描）
	// 注意: 5000 行上限为 Tier-0 内存预算约束，后续考虑配置化
	rows, err := hr.db.QueryContext(ctx, `
		SELECT id, doc_id, content, taint_level, taint_source, embedding
		FROM rag_chunks
		WHERE embedding IS NOT NULL AND embedding != '' AND deleted_at IS NULL
		LIMIT 5000
	`)
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
		var taintSource sql.NullString
		var embJSON sql.NullString
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource, &embJSON); err != nil {
			continue
		}
		if taintSource.Valid {
			chunk.TaintSource = taintSource.String
		}
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

// parseEmbedding 解析 JSON 格式的 float32 数组（[0.1,0.2,...]）。
func parseEmbedding(s string) ([]float32, error) {
	// 使用简单 JSON 解析
	var vals []float32
	// 手动解析: "[f,f,f,...]"
	if len(s) < 2 || s[0] != '[' {
		return nil, apperr.New(apperr.CodeInvalidInput, "invalid embedding format")
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return nil, nil
	}
	// 按逗号分割
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			token := strings.TrimSpace(s[start:i])
			if token != "" {
				vals = append(vals, float32(parseFloat(token)))
			}
			start = i + 1
		}
	}
	return vals, nil
}

// parseFloat 轻量 float 解析（无 strconv 依赖，Tier 0 Wasm 友好）。
func parseFloat(s string) float64 {
	i := 0
	neg := pfSign(s, &i)
	intPart := pfDigits(s, &i)
	fracPart := pfFrac(s, &i)
	exp := pfExp(s, &i)
	val := (intPart + fracPart) * math.Pow(10, exp)
	if neg {
		val = -val
	}
	return val
}

func pfSign(s string, i *int) bool {
	if *i < len(s) {
		if s[*i] == '-' {
			*i++
			return true
		} else if s[*i] == '+' {
			*i++
			return false
		}
	}
	return false
}

func pfDigits(s string, i *int) float64 {
	var v float64
	for *i < len(s) && s[*i] >= '0' && s[*i] <= '9' {
		v = v*10 + float64(s[*i]-'0')
		*i++
	}
	return v
}

func pfFrac(s string, i *int) float64 {
	var v float64
	if *i < len(s) && s[*i] == '.' {
		*i++
		scale := 0.1
		for *i < len(s) && s[*i] >= '0' && s[*i] <= '9' {
			v += float64(s[*i]-'0') * scale
			scale *= 0.1
			*i++
		}
	}
	return v
}

func pfExp(s string, i *int) float64 {
	if *i >= len(s) || (s[*i] != 'e' && s[*i] != 'E') {
		return 0
	}
	*i++
	neg := false
	if *i < len(s) && s[*i] == '-' {
		neg = true
		*i++
	} else if *i < len(s) && s[*i] == '+' {
		*i++
	}
	var exp float64
	for *i < len(s) && s[*i] >= '0' && s[*i] <= '9' {
		exp = exp*10 + float64(s[*i]-'0')
		*i++
	}
	if neg {
		return -exp
	}
	return exp
}

// rrfThreeWay 三路 RRF 融合（BM25×0.3 + Vector×0.6 + Graph×0.1）。
// M10 §2.2 HybridRetrieverConfig 权重。
func rrfThreeWay(bm25, vec, graph []Chunk, topK int) []Chunk {
	const k = 60
	scores := make(map[string]float64)
	chunkMap := make(map[string]Chunk)

	weightedRRF := func(results []Chunk, weight float64) {
		for rank, c := range results {
			scores[c.ID] += weight * (1.0 / float64(k+rank+1))
			if _, seen := chunkMap[c.ID]; !seen {
				chunkMap[c.ID] = c
			}
		}
	}

	weightedRRF(bm25, 0.3)
	weightedRRF(vec, 0.6)
	weightedRRF(graph, 0.1)

	type entry struct {
		id    string
		score float64
	}
	ranked := make([]entry, 0, len(scores))
	for id, s := range scores {
		ranked = append(ranked, entry{id, s})
	}
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && ranked[j].score > ranked[j-1].score; j-- {
			ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
		}
	}
	if topK > 0 && len(ranked) > topK {
		ranked = ranked[:topK]
	}
	out := make([]Chunk, len(ranked))
	for i, e := range ranked {
		out[i] = chunkMap[e.id]
	}
	return out
}
