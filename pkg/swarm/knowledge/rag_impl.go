package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// DefaultIngestionPipeline 实现了 IngestionPipeline，负责分块与打标污染等级
type DefaultIngestionPipeline struct {
	router *substrate.StorageRouter
}

func NewDefaultIngestionPipeline(router *substrate.StorageRouter) *DefaultIngestionPipeline {
	return &DefaultIngestionPipeline{
		router: router,
	}
}

func (p *DefaultIngestionPipeline) Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error) {
	if doc == nil {
		return nil, perrors.New(perrors.CodeInvalidInput, "document is nil")
	}

	docNode := &DocNode{
		ID:      fmt.Sprintf("doc_%s_%d", doc.Ref.ContentHash, time.Now().UnixNano()),
		Title:   doc.Ref.Title,
		Level:   0,
		Content: string(doc.Raw),
	}

	tree := &DocTree{
		Document:   docNode,
		SourceURL:  doc.Ref.URI,
		SourcePath: doc.Ref.URI,
	}

	chunks := p.chunkDocument(docNode.Content, docNode.ID, initialTaint, doc.Ref)

	store := p.router.Route(ctx, &substrate.StorageRequest{
		DataType:   "knowledge",
		AccessMode: "batch_write",
	})

	var ops []protocol.Op //nolint:prealloc
	for _, c := range chunks {
		data, _ := json.Marshal(c)
		ops = append(ops, protocol.Op{
			Key:   fmt.Appendf(nil, "chunk:%s:%s", docNode.ID, c.ID),
			Value: data,
			Type:  protocol.OpPut,
		})
	}

	docData, _ := json.Marshal(tree)
	ops = append(ops, protocol.Op{
		Key:   fmt.Appendf(nil, "doc:%s", docNode.ID),
		Value: docData,
		Type:  protocol.OpPut,
	})

	if err := store.BatchWrite(ctx, ops); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "failed to persist document and chunks", err)
	}

	return tree, nil
}

func (p *DefaultIngestionPipeline) Delete(ctx context.Context, uri string) error {
	store := p.router.Route(ctx, &substrate.StorageRequest{
		DataType:   "knowledge",
		AccessMode: "batch_write",
	})

	prefix := fmt.Appendf(nil, "chunk:%s:", uri)
	iter, err := store.Scan(ctx, prefix)
	if err != nil {
		return err
	}
	defer iter.Close()

	var ops []protocol.Op
	for iter.Next() {
		ops = append(ops, protocol.Op{
			Key:  iter.Key(),
			Type: protocol.OpDelete,
		})
	}

	ops = append(ops, protocol.Op{
		Key:  fmt.Appendf(nil, "doc:%s", uri),
		Type: protocol.OpDelete,
	})

	return store.BatchWrite(ctx, ops)
}

func (p *DefaultIngestionPipeline) chunkDocument(content string, docID string, taintLevel int, ref DocumentRef) []Chunk {
	var chunks []Chunk

	// 简单实现：按 1000 字符切分为 ParentChunk，按 250 字符切分为 LeafChunk
	parentSize := 1000
	leafSize := 250

	runes := []rune(content)
	chunkIndex := 0

	for i := 0; i < len(runes); i += parentSize {
		end := min(i+parentSize, len(runes))

		parentChunkID := fmt.Sprintf("pchunk_%s_%d", docID, i)
		parentChunk := Chunk{
			ID:          parentChunkID,
			Content:     string(runes[i:end]),
			DocID:       docID,
			SectionPath: []string{"root"},
			TaintLevel:  taintLevel,
			TaintSource: "ingestion",
			SourceURI:   ref.URI,
			DocVersion:  ref.ContentHash,
			ChunkType:   "parent",
			ChunkIndex:  chunkIndex,
		}
		chunkIndex++
		chunks = append(chunks, parentChunk)

		// 对 ParentChunk 进一步切分为 LeafChunk
		for j := i; j < end; j += leafSize {
			leafEnd := min(j+leafSize, end)
			leafChunkID := fmt.Sprintf("lchunk_%s_%d", docID, j)
			leafChunk := Chunk{
				ID:            leafChunkID,
				Content:       string(runes[j:leafEnd]),
				DocID:         docID,
				SectionPath:   []string{"root", parentChunkID},
				ParentChunkID: parentChunkID,
				TaintLevel:    taintLevel, // 污染标记传递，防止 Taint Washing
				TaintSource:   "ingestion",
				SourceURI:     ref.URI,
				DocVersion:    ref.ContentHash,
				ChunkType:     "leaf",
				ChunkIndex:    chunkIndex,
			}
			chunkIndex++
			chunks = append(chunks, leafChunk)
		}
	}

	return chunks
}

// DefaultHybridRetriever 实现了 HybridRetriever
type DefaultHybridRetriever struct {
	engine *substrate.HybridSearchEngine
}

func NewDefaultHybridRetriever(router *substrate.StorageRouter, embedder substrate.Embedder) *DefaultHybridRetriever {
	return &DefaultHybridRetriever{
		engine: substrate.NewHybridSearchEngine(router, embedder),
	}
}

func (r *DefaultHybridRetriever) Search(ctx context.Context, query *SearchQuery) ([]Chunk, error) {
	if query == nil || query.Text == "" {
		return nil, perrors.New(perrors.CodeInvalidInput, "empty query")
	}

	config := substrate.RetrievalConfig{
		BM25Weight:   0.3,
		VectorWeight: 0.6,
		GraphWeight:  0.1,
		RRFK:         60,
		OversampleN:  3,
		RerankTopM:   50,
		FinalTopK:    query.TopK,
	}
	if config.FinalTopK <= 0 {
		config.FinalTopK = 5
	}

	fragments, err := r.engine.Search(ctx, query.Text, []byte("chunk:"), config)
	if err != nil {
		return nil, err
	}

	var finalResults []Chunk //nolint:prealloc
	for _, f := range fragments {
		finalResults = append(finalResults, Chunk{
			ID:      f.Source,
			Content: f.Content,
		})
	}

	return finalResults, nil
}

// ContextExpander 将 LeafChunk 扩展为 AugmentedContext（父块 + 前后兄弟块）。
// 全 Tier 均启用，仅执行 DB 查询，无 LLM 调用。
type ContextExpander struct {
	router *substrate.StorageRouter
}

func NewContextExpander(router *substrate.StorageRouter) *ContextExpander {
	return &ContextExpander{router: router}
}

// Expand 给定一组 LeafChunk，返回带上下文的 AugmentedContext 列表。
func (ce *ContextExpander) Expand(ctx context.Context, chunks []Chunk) ([]AugmentedContext, error) {
	results := make([]AugmentedContext, 0, len(chunks))
	for _, leaf := range chunks {
		aug := AugmentedContext{Primary: leaf}

		db, err := ce.router.GetPrimary()
		if err != nil {
			results = append(results, aug)
			continue
		}

		// 查父块（同 DocID，ChunkType='parent'，section_path 前缀匹配）
		row := db.QueryRowContext(ctx,
			`SELECT id, doc_id, content, section_path, taint_level, taint_source, source_uri, doc_version
             FROM rag_chunks WHERE doc_id=? AND chunk_type='parent' AND id != ? LIMIT 1`,
			leaf.DocID, leaf.ID)
		var parent Chunk
		var sectionPath string
		if err := row.Scan(&parent.ID, &parent.DocID, &parent.Content,
			&sectionPath, &parent.TaintLevel, &parent.TaintSource,
			&parent.SourceURI, &parent.DocVersion); err == nil {
			// 反序列化 SectionPath（存储为逗号分隔字符串）
			parent.SectionPath = strings.Split(sectionPath, ",")
			aug.Parent = &parent
		}

		// 查前一个兄弟（同 DocID、同父、chunk_index < 当前）
		// 查后一个兄弟（同 DocID、同父、chunk_index > 当前）
		// 注：chunk_index 需在 rag_chunks 表中存在；若无则跳过
		results = append(results, aug)
	}
	return results, nil
}

// StructuredNavigator 在摘要索引中导航，定位目标 DocNode 后再做内容检索。
// 仅当 FeatureDeepRAG 开启时调用。
type StructuredNavigator struct {
	router   *substrate.StorageRouter
	embedder substrate.Embedder
}

func NewStructuredNavigator(router *substrate.StorageRouter, embedder substrate.Embedder) *StructuredNavigator {
	return &StructuredNavigator{router: router, embedder: embedder}
}

// Navigate 返回与 query 最相关的 docScope（docID 或 ""=全局降级）。
func (sn *StructuredNavigator) Navigate(ctx context.Context, query string) (string, error) {
	emb32 := sn.embedder.Embed(query)
	if len(emb32) == 0 {
		return "", nil // 失败降级：全文搜索
	}
	emb := make([]float64, len(emb32))
	for i, v := range emb32 {
		emb[i] = float64(v)
	}

	db, err := sn.router.GetPrimary()
	if err != nil {
		return "", nil //nolint:nilerr
	}

	// 在 rag_chunks 中找 chunk_type='summary' 的摘要块，按向量距离排序
	// 此处使用简化的余弦相似度（实际可接 SurrealDB vec_search）
	rows, err := db.QueryContext(ctx,
		`SELECT doc_id, embedding FROM rag_chunks WHERE chunk_type='summary' LIMIT 100`)
	if err != nil {
		return "", nil //nolint:nilerr
	}
	defer rows.Close()

	bestDocID := ""
	bestScore := 0.5 // RelevanceScore 低于此值时 fallback 全文搜索
	for rows.Next() {
		var docID string
		var embBytes []byte
		if err := rows.Scan(&docID, &embBytes); err != nil {
			continue
		}
		// 计算余弦相似度（embedding 存储为 JSON float64 数组）
		var docEmb []float64
		if err := json.Unmarshal(embBytes, &docEmb); err != nil {
			continue
		}
		score := cosineSimilarity(emb, docEmb)
		if score > bestScore {
			bestScore = score
			bestDocID = docID
		}
	}
	return bestDocID, nil // "" 表示降级全文搜索
}

// cosineSimilarity 计算两个向量的余弦相似度（内联辅助函数）。
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// QueryPlanner 将复杂查询分解为子查询。
// 仅当 FeatureDeepRAG 开启且 query token 数 >=30 时调用。
type QueryPlanner struct {
	provider protocol.Provider
}

func NewQueryPlanner(provider protocol.Provider) *QueryPlanner {
	return &QueryPlanner{provider: provider}
}

// Plan 将 query 分解为 1-5 个子查询。简单查询（<30 tokens）直接返回原查询。
func (qp *QueryPlanner) Plan(ctx context.Context, query string) ([]SubQuery, error) {
	if len(strings.Fields(query)) < 30 || qp.provider == nil {
		return []SubQuery{{Text: query, Weight: 1.0}}, nil
	}

	resp, err := qp.provider.Infer(ctx, []protocol.Message{
		{Role: "system", Content: `将用户查询分解为 2-5 个独立子查询以提升检索覆盖度。
严格按以下 JSON 格式输出，不加任何额外文字：
[{"text":"子查询1","scope":"","weight":0.6},{"text":"子查询2","scope":"","weight":0.4}]
weight 之和必须为 1.0，scope 为空表示全局检索。`},
		{Role: "user", Content: query},
	}, protocol.WithModel("standard"))
	if err != nil {
		return []SubQuery{{Text: query, Weight: 1.0}}, nil //nolint:nilerr // 失败降级单查询
	}

	var subs []SubQuery
	if err := json.Unmarshal([]byte(resp.Content), &subs); err != nil || len(subs) == 0 {
		return []SubQuery{{Text: query, Weight: 1.0}}, nil //nolint:nilerr
	}
	return subs, nil
}

// KnowledgeBase 是三阶段 RAG 的统一检索入口。
// Tier 0：HybridRetriever → ContextExpander
// Tier 1+（FeatureDeepRAG）：QueryPlanner → StructuredNavigator → HybridRetriever → ContextExpander
type KnowledgeBase struct {
	retriever   *DefaultHybridRetriever
	expander    *ContextExpander
	navigator   *StructuredNavigator // nil on Tier 0
	planner     *QueryPlanner        // nil on Tier 0
	featureGate interface {
		Enabled(observability.Feature) bool
	}
}

func NewKnowledgeBase(
	retriever *DefaultHybridRetriever,
	expander *ContextExpander,
	navigator *StructuredNavigator, // 传 nil 时自动降级
	planner *QueryPlanner, // 传 nil 时自动降级
	gate interface {
		Enabled(observability.Feature) bool
	},
) *KnowledgeBase {
	return &KnowledgeBase{
		retriever:   retriever,
		expander:    expander,
		navigator:   navigator,
		planner:     planner,
		featureGate: gate,
	}
}

// Search 执行分 Tier 的检索流程。
func (kb *KnowledgeBase) Search(ctx context.Context, req KnowledgeBaseSearchRequest) ([]AugmentedContext, error) {
	deepRAG := kb.featureGate != nil && kb.featureGate.Enabled(observability.FeatureDeepRAG) &&
		kb.planner != nil && kb.navigator != nil

	// 1. 查询分解（Tier 1+ only）
	subQueries := []SubQuery{{Text: req.Query, Weight: 1.0}}
	if deepRAG {
		subs, err := kb.planner.Plan(ctx, req.Query)
		if err == nil && len(subs) > 0 {
			subQueries = subs
		}
	}

	// 2. 每个子查询独立检索
	var allChunks []Chunk
	seen := map[string]struct{}{}
	for _, sub := range subQueries {
		scope := sub.TargetScope
		if deepRAG && scope == "" {
			// StructuredNavigator 自动定位 docScope
			if docID, err := kb.navigator.Navigate(ctx, sub.Text); err == nil {
				scope = docID
			}
		}
		sq := &SearchQuery{
			Text:     sub.Text,
			TopK:     req.TopK,
			DocScope: scope,
		}
		chunks, err := kb.retriever.Search(ctx, sq)
		if err != nil {
			continue
		}
		for _, c := range chunks {
			if _, dup := seen[c.ID]; !dup {
				seen[c.ID] = struct{}{}
				allChunks = append(allChunks, c)
			}
		}
	}

	// 3. ContextExpander（全 Tier）
	if len(allChunks) == 0 {
		return nil, nil
	}
	return kb.expander.Expand(ctx, allChunks)
}
