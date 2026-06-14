package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
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

	db, err := p.router.GetPrimary()
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "ingestion: get primary db failed", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "ingestion: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	docData, _ := json.Marshal(tree)
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO rag_docs (uri, doc_id, tree_json) VALUES (?, ?, ?)`,
		doc.Ref.URI, docNode.ID, string(docData),
	); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "ingestion: insert rag_docs", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO rag_chunks
			(id, doc_id, content, taint_level, taint_source, source_uri, doc_version,
			 chunk_seq, content_hash, embed_model_version, chunk_type, chunk_index)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "ingestion: prepare stmt", err)
	}
	defer stmt.Close()

	for i, c := range chunks {
		if _, err := stmt.ExecContext(ctx,
			c.ID, c.DocID, c.Content, c.TaintLevel, c.TaintSource,
			c.SourceURI, c.DocVersion, i, c.ContentHash, "", c.ChunkType, c.ChunkIndex,
		); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "ingestion: insert chunk", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "ingestion: commit", err)
	}

	return tree, nil
}

func (p *DefaultIngestionPipeline) Delete(ctx context.Context, uri string) error {
	db, err := p.router.GetPrimary()
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "delete: get primary db failed", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "delete: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	var docID string
	err = tx.QueryRowContext(ctx, `SELECT doc_id FROM rag_docs WHERE uri = ?`, uri).Scan(&docID)
	if err == nil && docID != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID); err != nil {
			return perrors.Wrap(perrors.CodeInternal, "delete: rag_chunks by doc_id", err)
		}
	} else {
		// fallback to delete by source_uri if doc_id not found
		if _, err := tx.ExecContext(ctx, `DELETE FROM rag_chunks WHERE source_uri = ?`, uri); err != nil {
			return perrors.Wrap(perrors.CodeInternal, "delete: rag_chunks by source_uri", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM rag_docs WHERE uri = ?`, uri); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "delete: rag_docs", err)
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

// StructuredNavigator 在摘要索引中导航，用 FTS5 BM25 定位最相关的 doc_id。
// 注：rag_chunks 表无 embedding 字段，向量在 SurrealDB-Core；此处使用 BM25 全文搜索。
type StructuredNavigator struct {
	router *substrate.StorageRouter
}

func NewStructuredNavigator(router *substrate.StorageRouter) *StructuredNavigator {
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
          AND rc.chunk_type = 'summary'
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
