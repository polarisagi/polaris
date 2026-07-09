package knowledge

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// DefaultHybridRetriever 实现了 HybridRetriever
type DefaultHybridRetriever struct {
	engine   *search.HybridSearchEngine
	reranker search.Reranker // 可 nil；FeatureDeepRAG 门控下注入 ApproximateColBERTReranker（2026-07-04 补齐）
}

// NewDefaultHybridRetriever 创建默认检索器。reranker 可传 nil（等价于不重排，
// 与改造前行为一致）。
func NewDefaultHybridRetriever(router *store.StorageRouter, embedder search.Embedder, reranker search.Reranker) *DefaultHybridRetriever {
	return &DefaultHybridRetriever{
		engine:   search.NewHybridSearchEngine(router, embedder),
		reranker: reranker,
	}
}

// Engine 暴露内部 HybridSearchEngine，供启动流程调用 Stats().RestoreStatsFromDB/FlushTo
// 恢复/持久化 CorpusStats（2026-07-04 审计补齐，任务18）。
func (r *DefaultHybridRetriever) Engine() *search.HybridSearchEngine {
	return r.engine
}

func (r *DefaultHybridRetriever) Search(ctx context.Context, query *SearchQuery) ([]Chunk, error) {
	if query == nil || query.Text == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "empty query")
	}

	config := search.RetrievalConfig{
		BM25Weight:   0.3,
		VectorWeight: 0.6,
		GraphWeight:  0.1,
		RRFK:         60,
		OversampleN:  3,
		RerankTopM:   50,
		FinalTopK:    query.TopK,
		Reranker:     r.reranker,
	}
	if config.FinalTopK <= 0 {
		config.FinalTopK = 5
	}

	fragments, err := r.engine.Search(ctx, query.Text, []byte("chunk:"), config)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "DefaultHybridRetriever.Search", err)
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
	router *store.StorageRouter
}

func NewContextExpander(router *store.StorageRouter) *ContextExpander {
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
             FROM rag_chunks WHERE doc_id=? AND chunk_type='parent' AND id != ? AND deleted_at IS NULL LIMIT 1`,
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

// StructuredNavigator 在摘要索引中导航，用 FTS5 BM25 定位最相关的 doc_id。
// 注：rag_chunks 表无 embedding 字段，向量在 SurrealDB-Core；此处使用 BM25 全文搜索。
type StructuredNavigator struct {
	router *store.StorageRouter
}

func NewStructuredNavigator(router *store.StorageRouter) *StructuredNavigator {
	return &StructuredNavigator{router: router}
}

// Navigate 用 FTS5 在 summary 块中全文搜索，返回最相关的 doc_id（""=降级全文搜索）。
func (sn *StructuredNavigator) Navigate(ctx context.Context, query string) (string, error) {
	if query == "" {
		return "", nil
	}
	db, err := sn.router.GetPrimary()
	if err != nil {
		return "", nil //nolint:nilerr
	}

	// FTS5 全文搜索 summary 块，取 BM25 rank 最高的 doc_id
	// summary 块在摘要生成完成前为空，此时返回 "" 自动降级全文搜索
	row := db.QueryRowContext(ctx, `
        SELECT rc.doc_id
        FROM rag_chunks_fts fts
        JOIN rag_chunks rc ON rc.rowid = fts.rowid
        WHERE rag_chunks_fts MATCH ?
          AND rc.chunk_type = 'summary' AND rc.deleted_at IS NULL
        ORDER BY rank
        LIMIT 1`, query)

	var docID string
	if err := row.Scan(&docID); err != nil {
		return "", nil //nolint:nilerr
	}
	return docID, nil
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

	resp, err := safecall.Infer(ctx, qp.provider, []types.Message{
		{Role: "system", Content: `将用户查询分解为 2-5 个独立子查询以提升检索覆盖度。
严格按以下 JSON 格式输出，不加任何额外文字：
[{"text":"子查询1","scope":"","weight":0.6},{"text":"子查询2","scope":"","weight":0.4}]
weight 之和必须为 1.0，scope 为空表示全局检索。`},
		{Role: "user", Content: query},
	}, types.WithModel("standard"))
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
// <8GB VPS（FeatureDeepRAG disabled）：HybridRetriever → ContextExpander
// Tier 0+（≥8GB，FeatureDeepRAG enabled）：QueryPlanner → StructuredNavigator → HybridRetriever → ContextExpander
type KnowledgeBase struct {
	retriever   HybridRetriever
	expander    *ContextExpander
	navigator   *StructuredNavigator      // nil when FeatureDeepRAG disabled (<8GB VPS)
	planner     *QueryPlanner             // nil when FeatureDeepRAG disabled (<8GB VPS)
	arbiter     *KnowledgeConflictArbiter // 冲突仲裁器，nil 时跳过仲裁
	featureGate interface {
		IsEnabled(probe.Feature) bool
	}
}

func NewKnowledgeBase(
	retriever HybridRetriever,
	expander *ContextExpander,
	navigator *StructuredNavigator, // 传 nil 时自动降级（<8GB VPS 或 FeatureDeepRAG 未启用）
	planner *QueryPlanner, // 传 nil 时自动降级
	arbiter *KnowledgeConflictArbiter,
	gate interface {
		IsEnabled(probe.Feature) bool
	},
) *KnowledgeBase {
	return &KnowledgeBase{
		retriever:   retriever,
		expander:    expander,
		navigator:   navigator,
		planner:     planner,
		arbiter:     arbiter,
		featureGate: gate,
	}
}

// Search 执行分 Tier 的检索流程。
//
//nolint:gocyclo
func (kb *KnowledgeBase) Search(ctx context.Context, req KnowledgeBaseSearchRequest) ([]AugmentedContext, error) {
	deepRAG := kb.featureGate != nil && kb.featureGate.IsEnabled(probe.FeatureDeepRAG) &&
		kb.planner != nil && kb.navigator != nil

	// 1. 查询分解（FeatureDeepRAG，Tier 0+/≥8GB）
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

	// 2.5 冲突仲裁（arbiter != nil 时启用）：移除低权威冲突 chunk
	if kb.arbiter != nil && len(allChunks) > 1 {
		allChunks = kb.arbiter.ArbitrateChunks(allChunks)
	}

	// 3. ContextExpander（全 Tier）
	if len(allChunks) == 0 {
		return nil, nil
	}
	return kb.expander.Expand(ctx, allChunks)
}
