package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/knowledge/graphrag"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func simpleHash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// PipelineImpl 实现 IngestionPipeline (M10 Knowledge RAG 摄取管道)。
// 生产实现: 将文本分块落盘到 SQLite rag_chunks 表（FTS5 支持），
// 同时将 DocTree 元数据持久化到 rag_docs 表（JSON 序列化）。
type PipelineImpl struct {
	db           protocol.SQLQuerier
	provider     protocol.Provider
	outboxWriter protocol.OutboxWriter
}

var _ IngestionPipeline = (*PipelineImpl)(nil)

// NewPipeline 创建 PipelineImpl，并确保 rag_docs 表存在。
func NewPipeline(db protocol.SQLQuerier, provider protocol.Provider, outboxWriter protocol.OutboxWriter) *PipelineImpl {
	// CREATE TABLE IF NOT EXISTS，幂等；上线前阶段可随主 schema 建表
	_, _ = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS rag_docs (
			uri          TEXT PRIMARY KEY,
			title        TEXT NOT NULL DEFAULT '',
			source_type  TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL DEFAULT '',
			tree_json    TEXT NOT NULL DEFAULT '{}',
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	return &PipelineImpl{db: db, provider: provider, outboxWriter: outboxWriter}
}

// Ingest 将文档转换为 Chunk 并持久化。
func (p *PipelineImpl) Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error) { //nolint:gocyclo
	if doc == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "knowledge: doc is nil")
	}

	// 增量检测：hash 相同则跳过重摄取，返回缓存 DocTree
	var existingHash string
	_ = p.db.QueryRowContext(ctx,
		`SELECT content_hash FROM rag_docs WHERE uri = ? AND deleted_at IS NULL`, doc.Ref.URI,
	).Scan(&existingHash)
	if existingHash != "" && existingHash == doc.Ref.ContentHash {
		var treeJSON string
		if err := p.db.QueryRowContext(ctx,
			`SELECT tree_json FROM rag_docs WHERE uri = ? AND deleted_at IS NULL`, doc.Ref.URI,
		).Scan(&treeJSON); err == nil && treeJSON != "" {
			var cached DocTree
			if json.Unmarshal([]byte(treeJSON), &cached) == nil {
				return &cached, nil
			}
		}
	}

	content := string(doc.Raw)

	var chunker ChunkStrategy
	switch doc.Ref.SourceType {
	case "md", "markdown":
		chunker = &MarkdownChunker{}
	case "go", "py", "python", "js", "ts", "javascript", "typescript", "java", "cpp", "c", "rs", "rust":
		chunker = &CodeChunker{}
	default:
		chunker = &PlainTextChunker{}
	}
	parts := chunker.Chunk(content, doc.Ref.SourceType)

	var nodes []*DocNode //nolint:prealloc

	for i, part := range parts {
		if part == "" {
			continue
		}
		contentHash := fmt.Sprintf("%x", simpleHash(part))
		chunkID := fmt.Sprintf("%s_chunk_%d", doc.Ref.ContentHash, i)

		nodes = append(nodes, &DocNode{
			ID:      chunkID,
			Title:   fmt.Sprintf("Chunk %d", i),
			Level:   1,
			Content: part,
		})

		// 持久化 Chunk 到 SQLite rag_chunks 表（含 inv_M10_03 lineage 字段）
		_, err := p.db.ExecContext(ctx, `
			INSERT INTO rag_chunks (id, doc_id, content, taint_level, taint_source,
				source_uri, doc_version, chunk_seq, content_hash, embed_model_version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '')
			ON CONFLICT(id) DO UPDATE SET
				content=excluded.content,
				taint_level=excluded.taint_level,
				taint_source=excluded.taint_source,
				source_uri=excluded.source_uri,
				doc_version=excluded.doc_version,
				chunk_seq=excluded.chunk_seq,
				content_hash=excluded.content_hash,
				created_at=CURRENT_TIMESTAMP
		`, chunkID, doc.Ref.URI, part, initialTaint, "",
			doc.Ref.URI, doc.Ref.ContentHash, i, contentHash)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "ingester: insert chunk failed", err)
		}
	}

	tree := &DocTree{
		Document: &DocNode{
			ID:       doc.Ref.URI,
			Title:    doc.Ref.Title,
			Level:    0,
			Children: nodes,
		},
		SourceURL: doc.Ref.URI,
	}

	// 持久化 DocTree 到 rag_docs 表（JSON 序列化）
	treeJSON, _ := json.Marshal(tree)
	if _, docErr := p.db.ExecContext(ctx, `
		INSERT INTO rag_docs (uri, title, source_type, content_hash, tree_json, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(uri) DO UPDATE SET
			title        = excluded.title,
			source_type  = excluded.source_type,
			content_hash = excluded.content_hash,
			tree_json    = excluded.tree_json,
			updated_at   = CURRENT_TIMESTAMP
	`, doc.Ref.URI, doc.Ref.Title, doc.Ref.SourceType, doc.Ref.ContentHash, string(treeJSON)); docErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ingester: upsert rag_docs failed", docErr)
	}

	// LLM 摘要生成（Tier 1+，provider 可用时）
	// 为每个 ParentChunk 生成 summary 类型块，供 StructuredNavigator 的 FTS5 导航使用。
	if p.provider != nil && len(nodes) > 0 {
		for _, node := range nodes {
			if node.Level != 1 || node.Content == "" {
				continue
			}
			summaryID := node.ID + "_summary"
			resp, err := p.provider.Infer(ctx, []types.Message{
				{Role: "system", Content: "用一句话（50字以内）总结以下文本片段的核心内容，输出纯文本，不加任何格式："},
				{Role: "user", Content: node.Content},
			}, types.WithModel("standard"))
			if err != nil || resp.Content == "" {
				continue
			}
			summaryHash := fmt.Sprintf("%x", simpleHash(resp.Content))
			if _, err := p.db.ExecContext(ctx, `
				INSERT INTO rag_chunks (id, doc_id, content, taint_level, taint_source,
					source_uri, doc_version, chunk_seq, content_hash, embed_model_version, chunk_type)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', 'summary')
				ON CONFLICT(id) DO UPDATE SET
					content=excluded.content,
					content_hash=excluded.content_hash,
					created_at=CURRENT_TIMESTAMP
			`, summaryID, doc.Ref.URI, resp.Content, initialTaint, "llm_summary",
				doc.Ref.URI, doc.Ref.ContentHash, -1, summaryHash); err != nil {
				slog.WarnContext(ctx, "knowledge_ingester: db write failed", "error", err)
			}
		}
	}

	// 异步触发 GraphBuild（Outbox 解耦）
	if p.outboxWriter != nil {
		payload, _ := json.Marshal(map[string]string{"doc_id": doc.Ref.URI})
		_ = p.outboxWriter.Write(ctx, protocol.OutboxEntry{
			TargetEngine: graphrag.EventTypeRAGDocIngested,
			Payload:      payload,
		})
	}

	return tree, nil
}

// Delete 删除指定文档的所有片段。
func (p *PipelineImpl) Delete(ctx context.Context, uri string) error {
	_, err := p.db.ExecContext(ctx, "UPDATE rag_chunks SET deleted_at = CURRENT_TIMESTAMP WHERE doc_id = ?", uri)
	if err != nil {
		return fmt.Errorf("PipelineImpl.Delete: %w", err)
	}
	return nil
}
