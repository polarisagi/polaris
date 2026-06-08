package knowledge

import (
	"context"
	"database/sql"
	"math"
	"sort"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// VectorEmbedder 向量嵌入接口（consumer-side，防止包循环）。
// Tier 0 可传 nil，降级为纯 FTS5；Tier 1+ 注入 substrate.EmbeddingBatcher 实现。
type VectorEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// HybridRetrieverImpl 实现 HybridRetriever。
// 检索策略:
//   - Tier 0 (embedder=nil): FTS5 BM25 单路，按 rank 排序。
//   - Tier 1+ (embedder 非 nil): FTS5 + Dense Vector 双路 RRF 融合。
type HybridRetrieverImpl struct {
	db       *sql.DB
	embedder VectorEmbedder // optional，nil = FTS5 only
}

var _ HybridRetriever = (*HybridRetrieverImpl)(nil)

// NewHybridRetriever 创建 FTS5-only 检索器（Tier 0）。
func NewHybridRetriever(db *sql.DB) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{db: db}
}

// NewHybridRetrieverWithEmbedder 创建含密集向量路径的检索器（Tier 1+）。
func NewHybridRetrieverWithEmbedder(db *sql.DB, embedder VectorEmbedder) *HybridRetrieverImpl {
	return &HybridRetrieverImpl{db: db, embedder: embedder}
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
		return nil, err
	}

	// 向量路径（Tier 1+）
	if hr.embedder != nil {
		vecResults, vecErr := hr.searchVector(ctx, query.Text, topK*3)
		if vecErr == nil && len(vecResults) > 0 {
			// RRF 融合 (k=60)
			return rrf(ftsResults, vecResults, topK), nil
		}
		// 向量路径失败时降级为 FTS5 结果
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
		)
	`
	rows, err := hr.db.QueryContext(ctx, sqlQuery, queryText, limit)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "hybrid_retriever: fts search failed", err)
	}
	defer rows.Close()

	var results []Chunk
	for rows.Next() {
		var chunk Chunk
		var taintSource sql.NullString
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Content, &chunk.TaintLevel, &taintSource); err != nil {
			return nil, err
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
func (hr *HybridRetrieverImpl) searchVector(ctx context.Context, queryText string, limit int) ([]Chunk, error) {
	queryEmbed, err := hr.embedder.Embed(ctx, queryText)
	if err != nil {
		return nil, err
	}
	if len(queryEmbed) == 0 {
		return nil, nil
	}

	// 读取所有有 embedding 的 chunk（生产环境应使用 ANN 索引；Tier 0 线性扫描）
	rows, err := hr.db.QueryContext(ctx, `
		SELECT id, doc_id, content, taint_level, taint_source, embedding
		FROM rag_chunks
		WHERE embedding IS NOT NULL AND embedding != ''
		LIMIT 5000
	`)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "hybrid_retriever: vector scan failed", err)
	}
	defer rows.Close()

	type scored struct {
		chunk Chunk
		score float64
	}
	var scored_ []scored

	for rows.Next() {
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
		return nil, err
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

// rrf Reciprocal Rank Fusion 融合两路结果，k=60。
func rrf(fts, vec []Chunk, topK int) []Chunk {
	const k = 60.0
	scores := make(map[string]float64)
	chunks := make(map[string]Chunk)

	addRank := func(results []Chunk, weight float64) {
		for rank, c := range results {
			scores[c.ID] += weight / (k + float64(rank) + 1)
			chunks[c.ID] = c
		}
	}
	addRank(fts, 1.0)
	addRank(vec, 0.8)

	type kv struct {
		id    string
		score float64
	}
	sorted := make([]kv, 0, len(scores))
	for id, s := range scores {
		sorted = append(sorted, kv{id, s})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].score > sorted[j].score })

	if len(sorted) > topK {
		sorted = sorted[:topK]
	}
	results := make([]Chunk, len(sorted))
	for i, kv := range sorted {
		results[i] = chunks[kv.id]
	}
	return results
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
		return nil, perrors.New(perrors.CodeInvalidInput, "invalid embedding format")
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return nil, nil
	}
	// 按逗号分割
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			token := s[start:i]
			var f float64
			for _, c := range token {
				if c == ' ' {
					continue
				}
				// 简单 strconv.ParseFloat 替代
				f = parseFloat(token)
				break
			}
			vals = append(vals, float32(f))
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
	if *i < len(s) && s[*i] == '-' {
		*i++
		return true
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
