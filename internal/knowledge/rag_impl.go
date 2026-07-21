package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"
	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// summaryInferConcurrency 单个 DefaultIngestionPipeline 实例上，摘要树生成
// （buildSummaryTree/generateSummaryLevels）允许同时在途的 provider.Infer
// 调用数上限（批次 8 H1 修复）。Ingest 为每篇文档摘要生成单独开一个
// concurrent.SafeGo 后台 goroutine，多篇文档并发摄入时若不加限制，会对
// LLM Provider 产生无界并发调用，构成潜在的重试风暴/限流雪崩风险。
const summaryInferConcurrency = 3

// DefaultIngestionPipeline 实现了 IngestionPipeline，负责分块与打标污染等级
type DefaultIngestionPipeline struct {
	router             *store.StorageRouter
	provider           protocol.Provider
	outboxWriter       protocol.OutboxWriter // 可选；nil 时降级为 goroutine
	searchEngine       *search.HybridSearchEngine
	summaryInferSem    chan struct{}                  // 限制并发摘要 Infer 调用数（H1）
	boundarySerializer *taint.TaintBoundarySerializer // 可选；nil 时不计算/校验 taint_hmac（inv_M11_02）
}

func NewDefaultIngestionPipeline(router *store.StorageRouter, provider protocol.Provider, outboxWriter protocol.OutboxWriter, searchEngine *search.HybridSearchEngine, boundarySerializer *taint.TaintBoundarySerializer) *DefaultIngestionPipeline {
	return &DefaultIngestionPipeline{
		router:             router,
		provider:           provider,
		outboxWriter:       outboxWriter,
		searchEngine:       searchEngine,
		summaryInferSem:    make(chan struct{}, summaryInferConcurrency),
		boundarySerializer: boundarySerializer,
	}
}

func (p *DefaultIngestionPipeline) Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error) {
	tracer := trace.NewTracer()
	span, ctx := tracer.StartSpan(ctx, trace.SpanMemoryOp, "Knowledge.Ingest")
	defer tracer.EndSpan(span)

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
			(id, doc_id, content, taint_level, taint_source, taint_hmac, source_uri, doc_version,
			 chunk_seq, content_hash, embed_model_version, chunk_type, chunk_index)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: prepare stmt", err)
	}
	defer stmt.Close()

	for i, c := range chunks {
		hmacHex := sealChunkTaint(p.boundarySerializer, c.ID, c.Content, c.TaintLevel, c.TaintSource)
		if _, err := stmt.ExecContext(ctx,
			c.ID, c.DocID, c.Content, c.TaintLevel, c.TaintSource, hmacHex,
			c.SourceURI, c.DocVersion, i, c.ContentHash, "", c.ChunkType, c.ChunkIndex,
		); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "ingestion: insert chunk", err)
		}
		if p.searchEngine != nil {
			_ = p.searchEngine.AddDocument(ctx, c.ID, c.Content)
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
		concurrent.SafeGo(context.Background(), "knowledge.rag.build_summary_tree", func(ctx context.Context) {
			p.buildSummaryTree(ctx, docNode, db)
		})
	}

	return tree, nil
}

// buildSummaryTree 见 rag_summary_tree.go（R7 拆分）。

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

	var paragraphs []string
	ext := ""
	if idx := strings.LastIndex(ref.URI, "."); idx != -1 {
		ext = strings.ToLower(ref.URI[idx+1:])
	}
	chunker := &DefaultChunker{}
	paragraphs = chunker.Chunk(content, ext)

	// Step 2: 段落合并为 ParentChunk (if paragraphs are small enough, they merge. if they are already big, they remain separate)
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

// GetRecentChunks returns a list of recent chunks.
// Task 4: SyntheticEvalGen Pipeline integration hook.
func (p *DefaultIngestionPipeline) GetRecentChunks(ctx context.Context, limit int) ([]string, error) {
	// 按照任务要求，不需要真查 DB，返回一个写死的一条 Chunk 的 slice
	return []string{"This is a mocked recent chunk for SyntheticEvalGen pipeline test."}, nil
}
