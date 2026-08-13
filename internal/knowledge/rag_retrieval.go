package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// recordExplainBits 按位图上报每一路命中的检索来源指标（Batch8 ExplainBits 归因修复）。
// internal/knowledge 的三路召回固定为 BM25/Vector/Graph，不涉及
// Simhash/Reflection/Durative/Semantic（那几路是 internal/memory/retrieval 独有的召回策略）。
func recordExplainBits(ctx context.Context, bits uint8) {
	if bits&types.BitBM25 != 0 {
		metrics.RecordExplainBit(ctx, "BM25")
	}
	if bits&types.BitVector != 0 {
		metrics.RecordExplainBit(ctx, "Vector")
	}
	if bits&types.BitGraph != 0 {
		metrics.RecordExplainBit(ctx, "Graph")
	}
}

// tier0DefaultTopK Tier-0 路径未指定 FinalTopK 时的默认返回条数。
// 刻意低于 Tier-1 的 defaultKnowledgeTopK(10)：<8GB 部署上每条 chunk 都要
// 进 Prompt，上下文预算更紧。
const tier0DefaultTopK = 5

type ragReranker interface {
	Rerank(ctx context.Context, query string, docs []types.ScoredFragment) []types.ScoredFragment
}

// DefaultHybridRetriever 实现了 HybridRetriever
type DefaultHybridRetriever struct {
	engine   *search.HybridSearchEngine
	reranker ragReranker // 可 nil；FeatureDeepRAG 门控下注入 ApproximateColBERTReranker（2026-07-04 补齐）
}

// NewDefaultHybridRetriever 创建默认检索器。reranker 可传 nil（等价于不重排，
// 与改造前行为一致）。
func NewDefaultHybridRetriever(router *store.StorageRouter, embedder search.Embedder, reranker ragReranker) *DefaultHybridRetriever {
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

// tier0RerankTopM Tier-0 路径送入 Cross-encoder 重排的候选上限（M10 §2.2）。
const tier0RerankTopM = 50

// Search 执行 Tier-0（StorageRouter-only，<8GB VPS）路径的混合检索。
//
// GD-13-002 收敛：此前本方法调用 HybridSearchEngine.Search + RRFFuse，
// 那是全仓**第三套**独立的融合实现（另两套在 memory/retrieval 与
// knowledge/retriever.go），且恰好服务于最受资源约束的 Tier-0 部署。
// 现统一走 search.HybridSearch，与 SurrealDB 路径（HybridRetrieverImpl）
// 共用同一份 RRF/ExplainBits/Rerank/TopK 逻辑与同一组 M10 §2.2 权重常量。
func (r *DefaultHybridRetriever) Search(ctx context.Context, query string, scope types.SearchScope, config types.RetrievalConfig) ([]types.ScoredFragment, error) {
	req := config
	_ = req.TaintMax
	if query == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "empty query")
	}

	topK := config.FinalTopK
	if topK <= 0 {
		topK = tier0DefaultTopK
	}

	var queryEmbed []float32
	if emb := r.engine.EmbedQuery(query); len(emb) > 0 {
		queryEmbed = emb
	}

	var searchReranker search.Reranker
	if r.reranker != nil {
		searchReranker = &tier0RerankerAdapter{inner: r.reranker, topM: tier0RerankTopM}
	}

	merged, err := search.HybridSearch(ctx,
		r.engine.NewRouterDocumentSource([]byte("chunk:"), config.Tier0VectorScanLimit),
		query, queryEmbed,
		search.HybridSearchConfig{
			SingleRouteTimeoutMs: r.engine.Config().Thresholds.M10Knowledge.SingleRouteTimeoutMs,
			// 与 knowledge/source.go 同一组常量：Tier-0 与 Tier-1 的检索
			// 策略必须一致，只是底层召回能力不同（全表扫描 vs FTS5/HNSW）。
			BM25Weight:   knowledgeBM25Weight,
			VectorWeight: knowledgeVectorWeight,
			GraphWeight:  knowledgeGraphWeight,
			RRFk:         knowledgeRRFk,
			TopK:         topK,
			RecallWidth:  topK * 3,
			EnableRerank: searchReranker != nil,
			Reranker:     searchReranker,
		})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "DefaultHybridRetriever.Search", err)
	}

	for i := range merged {
		// 污点等级从 Metadata 还原：StorageRouter 路径的文档以 JSON 存储，
		// taint_level 在 Metadata 里而非独立字段。
		if merged[i].TaintLevel == 0 && merged[i].Metadata != nil {
			merged[i].TaintLevel = parseTaintLevel(merged[i].Metadata["taint_level"])
		}
		recordExplainBits(ctx, merged[i].ExplainBits)
	}
	return merged, nil
}

// tier0RerankerAdapter 把 Tier-0 的 ragReranker（无 error 返回）适配为统一管线的
// search.Reranker，并施加 topM 候选截断——Cross-encoder 成本随候选数线性增长，
// Tier-0（<8GB）不能把全部融合结果都送进去。
type tier0RerankerAdapter struct {
	inner ragReranker
	topM  int
}

func (a *tier0RerankerAdapter) Rerank(ctx context.Context, query string, docs []types.ScoredFragment) ([]types.ScoredFragment, error) {
	if a.inner == nil || len(docs) == 0 {
		return docs, nil
	}
	candidates := docs
	var tail []types.ScoredFragment
	if a.topM > 0 && len(candidates) > a.topM {
		tail = candidates[a.topM:]
		candidates = candidates[:a.topM]
	}
	// 重排头部候选，未参与重排的尾部原样接回——直接丢弃会让 TopK 截断
	// 在候选数略多于 topM 时凭空少掉结果。
	return append(a.inner.Rerank(ctx, query, candidates), tail...), nil
}

// ContextExpander 将 LeafChunk 扩展为 AugmentedContext（父块 + 前后兄弟块）。
// 全 Tier 均启用，仅执行 DB 查询，无 LLM 调用。
type ContextExpander struct {
	router             *store.StorageRouter
	boundarySerializer *taint.TaintBoundarySerializer // 可选；nil 时不校验 taint_hmac（inv_M11_02）
}

func NewContextExpander(router *store.StorageRouter) *ContextExpander {
	return &ContextExpander{router: router}
}

// SetBoundarySerializer 注入跨边界 HMAC 校验器（inv_M11_02），与
// HybridRetrieverImpl.SetBoundarySerializer 同一模式的启动期热注入 setter。
func (ce *ContextExpander) SetBoundarySerializer(ser *taint.TaintBoundarySerializer) {
	ce.boundarySerializer = ser
}

// Expand 给定一组 LeafChunk，返回带上下文的 AugmentedContext 列表。
func (ce *ContextExpander) Expand(ctx context.Context, chunks []Chunk, taintMax types.TaintLevel) ([]AugmentedContext, error) {
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
			`SELECT id, doc_id, content, section_path, taint_level, taint_source, taint_hmac, source_uri, doc_version
             FROM rag_chunks WHERE doc_id=? AND chunk_type='parent' AND id != ? AND deleted_at IS NULL LIMIT 1`,
			leaf.DocID, leaf.ID)
		var parent Chunk
		var sectionPath, taintHMAC string
		if err := row.Scan(&parent.ID, &parent.DocID, &parent.Content,
			&sectionPath, &parent.TaintLevel, &parent.TaintSource, &taintHMAC,
			&parent.SourceURI, &parent.DocVersion); err == nil {
			// 反序列化 SectionPath（存储为逗号分隔字符串）
			parent.SectionPath = strings.Split(sectionPath, ",")
			parent.TaintLevel = verifyChunkTaint(ce.boundarySerializer, parent.ID, parent.Content, parent.TaintLevel, parent.TaintSource, taintHMAC)
			if types.TaintLevel(parent.TaintLevel) > taintMax {
				slog.DebugContext(ctx, "knowledge: parent chunk filtered by TaintMax", "chunk_id", parent.ID, "chunk_taint", parent.TaintLevel, "req_taint_max", taintMax)
				if metrics.InstrRAGTaintDrops != nil {
					metrics.InstrRAGTaintDrops.Add(ctx, 1)
				}
			} else {
				aug.Parent = &parent
			}
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
	req := config
	_ = req.TaintMax
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
        LIMIT 1`, util.QuoteFTS5Query(query))

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
	}, types.WithModelPool(string(types.ModelPoolDefault)))
	if err != nil || resp == nil {
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
	retriever   protocol.HybridRetriever
	expander    *ContextExpander
	navigator   *StructuredNavigator      // nil when FeatureDeepRAG disabled (<8GB VPS)
	planner     *QueryPlanner             // nil when FeatureDeepRAG disabled (<8GB VPS)
	arbiter     *KnowledgeConflictArbiter // 冲突仲裁器，nil 时跳过仲裁
	featureGate interface {
		IsEnabled(probe.Feature) bool
	}
}

func NewKnowledgeBase(
	retriever protocol.HybridRetriever,
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
	if req.TaintMax == 0 {
		return nil, apperr.New(apperr.CodeForbidden, "knowledge: TaintMax 未指定，按 fail-closed 拒绝检索")
	}

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
		scopeConfig := types.SearchScope{
			Type:    "document_tree",
			Subtree: scope,
		}
		chunks, err := kb.retriever.Search(ctx, sub.Text, scopeConfig, types.RetrievalConfig{FinalTopK: req.TopK})
		if err != nil {
			continue
		}
		for _, c := range chunks {
			if _, dup := seen[c.Source]; !dup {
				if types.TaintLevel(c.TaintLevel) > req.TaintMax {
					slog.DebugContext(ctx, "knowledge: chunk filtered by TaintMax", "chunk_id", c.Source, "chunk_taint", c.TaintLevel, "req_taint_max", req.TaintMax)
					if metrics.InstrRAGTaintDrops != nil {
						metrics.InstrRAGTaintDrops.Add(ctx, 1)
					}
					continue
				}
				seen[c.Source] = struct{}{}
				chunk := Chunk{
					ID:          c.Source,
					DocID:       c.Source,
					Content:     c.Content,
					TaintLevel:  int(c.TaintLevel),
					TaintSource: c.Metadata["taint_source"],
					SourceURI:   c.Source,
				}
				allChunks = append(allChunks, chunk)
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
	return kb.expander.Expand(ctx, allChunks, req.TaintMax)
}

func parseTaintLevel(s string) types.TaintLevel {
	if s == "" {
		return 0
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return types.TaintLevel(i)
}
