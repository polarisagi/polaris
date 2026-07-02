package knowledge

import (
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/types"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// DefaultIngestionPipeline 实现了 IngestionPipeline，负责分块与打标污染等级
type DefaultIngestionPipeline struct {
	router       *store.StorageRouter
	provider     protocol.Provider
	outboxWriter protocol.OutboxWriter // 可选；nil 时降级为 goroutine
}

func NewDefaultIngestionPipeline(router *store.StorageRouter, provider protocol.Provider, outboxWriter protocol.OutboxWriter) *DefaultIngestionPipeline {
	return &DefaultIngestionPipeline{
		router:       router,
		provider:     provider,
		outboxWriter: outboxWriter,
	}
}

func (p *DefaultIngestionPipeline) Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error) {
	if doc == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "document is nil")
	}

	db, err := p.router.GetPrimary()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: get primary db failed", err)
	}

	// 增量检测：hash 相同则跳过重摄取，返回缓存 DocTree
	var existingHash string
	_ = db.QueryRowContext(ctx,
		`SELECT content_hash FROM rag_docs WHERE uri = ?`, doc.Ref.URI,
	).Scan(&existingHash)
	if existingHash != "" && existingHash == doc.Ref.ContentHash {
		var treeJSON string
		if err := db.QueryRowContext(ctx,
			`SELECT tree_json FROM rag_docs WHERE uri = ?`, doc.Ref.URI,
		).Scan(&treeJSON); err == nil && treeJSON != "" {
			var cached DocTree
			if json.Unmarshal([]byte(treeJSON), &cached) == nil {
				return &cached, nil
			}
		}
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

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	docData, _ := json.Marshal(tree)
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO rag_docs (uri, doc_id, tree_json) VALUES (?, ?, ?)`,
		doc.Ref.URI, docNode.ID, string(docData),
	); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: insert rag_docs", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO rag_chunks
			(id, doc_id, content, taint_level, taint_source, source_uri, doc_version,
			 chunk_seq, content_hash, embed_model_version, chunk_type, chunk_index)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: prepare stmt", err)
	}
	defer stmt.Close()

	for i, c := range chunks {
		if _, err := stmt.ExecContext(ctx,
			c.ID, c.DocID, c.Content, c.TaintLevel, c.TaintSource,
			c.SourceURI, c.DocVersion, i, c.ContentHash, "", c.ChunkType, c.ChunkIndex,
		); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: insert chunk", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: commit", err)
	}

	if p.outboxWriter != nil {
		// 触发 LLM 摘要生成
		ev1, _ := protocol.NewOutboxEvent(graphrag.EventTypeRAGDocSummaryNeeded, "generate", map[string]string{"doc_id": docNode.ID}, "summary:"+docNode.ID)
		_ = p.outboxWriter.Write(ctx, ev1)
		// 触发知识图谱构建（GraphBuildOutboxHandler 监听此事件）
		ev2, _ := protocol.NewOutboxEvent(graphrag.EventTypeRAGDocIngested, "graph_build", map[string]string{"doc_id": docNode.ID}, "graph:"+docNode.ID)
		_ = p.outboxWriter.Write(ctx, ev2)
	} else {
		go p.buildSummaryTree(context.Background(), docNode, db)
	}

	return tree, nil
}

func (p *DefaultIngestionPipeline) buildSummaryTree(ctx context.Context, docNode *DocNode, db protocol.SQLQuerier) {
	if p.provider == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)

	// 查询所有 leaf chunks（段落级原始内容）
	rows, err := db.QueryContext(ctx,
		`SELECT id, content FROM rag_chunks WHERE doc_id = ? AND chunk_type = 'leaf' AND deleted_at IS NULL ORDER BY chunk_index ASC`,
		docNode.ID)
	if err != nil {
		return
	}
	defer rows.Close()

	type leafChunk struct{ id, content string }
	var leaves []leafChunk
	for rows.Next() {
		var lc leafChunk
		if err := rows.Scan(&lc.id, &lc.content); err == nil {
			leaves = append(leaves, lc)
		}
	}
	if len(leaves) == 0 {
		return
	}

	// 查询源 chunks 最高污点级别（taint 只升不降）
	var srcTaint int
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(taint_level), 0) FROM rag_chunks WHERE doc_id = ? AND deleted_at IS NULL`,
		docNode.ID).Scan(&srcTaint)

	summarize := func(prompt string, maxTokens int) string {
		sCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		resp, err := p.provider.Infer(sCtx, []types.Message{{Role: "user", Content: prompt}},
			types.WithMaxTokens(maxTokens))
		if err != nil || resp == nil {
			return ""
		}
		return strings.TrimSpace(resp.Content)
	}

	insertSummary := func(id, content, chunkType string, idx int) {
		if content == "" {
			return
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO rag_chunks (id, doc_id, content, taint_level, taint_source, chunk_type, chunk_index, created_at)
             VALUES (?,?,?,?,?,?,?,?)`,
			id, docNode.ID, content, srcTaint, "auto_summary", chunkType, idx, now); err != nil {
			slog.WarnContext(ctx, "rag_impl: db write failed", "error", err)
		}
	}

	// L1 段落级摘要（每个 leaf chunk → ≤30 tokens 关键句）
	for i, leaf := range leaves {
		prompt := fmt.Sprintf("用一句话（≤30个token）总结以下段落的核心信息：\n%s", leaf.content)
		summary := summarize(prompt, 60)
		insertSummary(fmt.Sprintf("para_summary_%s_%d", docNode.ID, i), summary, "para_summary", i)
	}

	// L2 章节级摘要（每 5 个 leaf 为一组 → ~100 tokens）
	groupSize := 5
	for gi := 0; gi < len(leaves); gi += groupSize {
		end := min(gi+groupSize, len(leaves))
		group := leaves[gi:end]
		combined := make([]string, len(group))
		for k, lc := range group {
			combined[k] = lc.content
		}
		prompt := fmt.Sprintf("将以下段落内容总结为约100个token的章节摘要：\n%s",
			strings.Join(combined, "\n\n"))
		summary := summarize(prompt, 150)
		insertSummary(fmt.Sprintf("chap_summary_%s_%d", docNode.ID, gi/groupSize), summary, "chap_summary", gi/groupSize)
	}

	// L3 文档级摘要（全文 → ≤200 tokens）
	allContent := make([]string, len(leaves))
	for i, lc := range leaves {
		allContent[i] = lc.content
	}
	joined := strings.Join(allContent, "\n\n")
	if len(joined) > 8000 { // 避免超长 prompt
		joined = joined[:8000] + "…"
	}
	prompt := fmt.Sprintf("请生成一个不超过200个token的文档级摘要：\n%s", joined)
	docSummary := summarize(prompt, 300)
	insertSummary(fmt.Sprintf("doc_summary_%s", docNode.ID), docSummary, "doc_summary", -1)
}

func (p *DefaultIngestionPipeline) Delete(ctx context.Context, uri string) error {
	db, err := p.router.GetPrimary()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "delete: get primary db failed", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "delete: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)

	// 软删除 rag_chunks（Tombstone: deleted_at 时间戳）
	var docID string
	err = tx.QueryRowContext(ctx, `SELECT doc_id FROM rag_docs WHERE uri = ? AND deleted_at IS NULL`, uri).Scan(&docID)
	if err == nil && docID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE rag_chunks SET deleted_at = ? WHERE doc_id = ? AND deleted_at IS NULL`,
			now, docID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "delete: tombstone rag_chunks by doc_id", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE rag_chunks SET deleted_at = ? WHERE source_uri = ? AND deleted_at IS NULL`,
			now, uri); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "delete: tombstone rag_chunks by source_uri", err)
		}
	}

	// 软删除 rag_docs
	if _, err := tx.ExecContext(ctx,
		`UPDATE rag_docs SET deleted_at = ? WHERE uri = ? AND deleted_at IS NULL`,
		now, uri); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "delete: tombstone rag_docs", err)
	}

	return tx.Commit()
}

func (p *DefaultIngestionPipeline) chunkDocument(content string, docID string, taintLevel int, ref DocumentRef) []Chunk {
	const parentMaxRunes = 1000
	const leafMaxRunes = 250

	// Step 1: 段落切分
	paragraphs := splitParagraphs(content)

	// Step 2: 段落合并为 ParentChunk
	parents := mergeParagraphsIntoParents(paragraphs, parentMaxRunes)

	// Step 3: ParentChunk → LeafChunks（句子边界切分）
	var chunks []Chunk //nolint:prealloc
	chunkIndex := 0
	for pi, parentText := range parents {
		parentChunkID := fmt.Sprintf("pchunk_%s_%d", docID, pi)
		parentChunk := Chunk{
			ID:          parentChunkID,
			Content:     parentText,
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

		leaves := splitIntoLeaves(parentText, leafMaxRunes)
		for li, leafText := range leaves {
			leafChunkID := fmt.Sprintf("lchunk_%s_%d_%d", docID, pi, li)
			chunks = append(chunks, Chunk{
				ID:            leafChunkID,
				Content:       leafText,
				DocID:         docID,
				SectionPath:   []string{"root", parentChunkID},
				ParentChunkID: parentChunkID,
				TaintLevel:    taintLevel,
				TaintSource:   "ingestion",
				SourceURI:     ref.URI,
				DocVersion:    ref.ContentHash,
				ChunkType:     "leaf",
				ChunkIndex:    chunkIndex,
			})
			chunkIndex++
		}
	}
	return chunks
}

// splitParagraphs 以双换行切分段落，过滤空段落。
func splitParagraphs(content string) []string {
	parts := strings.Split(content, "\n\n")
	result := parts[:0]
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			result = append(result, s)
		}
	}
	return result
}

// mergeParagraphsIntoParents 将段落累积为 ParentChunk，不超过 maxRunes。
// 单段落超限时整段作为一个 parent（兜底：留给 leaf 强切）。
func mergeParagraphsIntoParents(paragraphs []string, maxRunes int) []string {
	var parents []string
	var buf []rune
	for _, para := range paragraphs {
		pr := []rune(para)
		if len(buf)+len(pr)+2 > maxRunes && len(buf) > 0 {
			parents = append(parents, string(buf))
			buf = buf[:0]
		}
		if len(pr) > maxRunes {
			if len(buf) > 0 {
				parents = append(parents, string(buf))
				buf = buf[:0]
			}
			// 超长单段落硬切分
			for start := 0; start < len(pr); start += maxRunes {
				end := start + maxRunes
				if end > len(pr) {
					end = len(pr)
				}
				parents = append(parents, string(pr[start:end]))
			}
			continue
		}
		if len(buf) > 0 {
			buf = append(buf, '\n', '\n')
		}
		buf = append(buf, pr...)
	}
	if len(buf) > 0 {
		parents = append(parents, string(buf))
	}
	return parents
}

// splitIntoLeaves 在句子边界切分文本为 LeafChunk，每个不超过 maxRunes。
// 句子结束符：。！？；（中文）和 ". " "! " "? "（英文，后接空格/EOF）。
func splitIntoLeaves(text string, maxRunes int) []string {
	runes := []rune(text)
	var leaves []string
	start := 0
	for start < len(runes) {
		end := min(start+maxRunes, len(runes))
		if end < len(runes) {
			// 在 [start, end] 内找最后一个句子结束符
			cut := -1
			for i := end - 1; i > start; i-- {
				r := runes[i]
				if r == '。' || r == '！' || r == '？' || r == '；' {
					cut = i + 1
					break
				}
				// 英文：结束符后跟空格
				if (r == '.' || r == '!' || r == '?') && i+1 < len(runes) && runes[i+1] == ' ' {
					cut = i + 2 // 包含空格
					break
				}
			}
			if cut > start {
				end = cut
			}
		}
		leaf := strings.TrimSpace(string(runes[start:end]))
		if leaf != "" {
			leaves = append(leaves, leaf)
		}
		start = end
	}
	return leaves
}

// DefaultHybridRetriever 实现了 HybridRetriever
type DefaultHybridRetriever struct {
	engine *search.HybridSearchEngine
}

func NewDefaultHybridRetriever(router *store.StorageRouter, embedder search.Embedder) *DefaultHybridRetriever {
	return &DefaultHybridRetriever{
		engine: search.NewHybridSearchEngine(router, embedder),
	}
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

	resp, err := qp.provider.Infer(ctx, []types.Message{
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
