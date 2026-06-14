package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"

	perrors "github.com/polarisagi/polaris/internal/errors"
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
	db *sql.DB
}

var _ IngestionPipeline = (*PipelineImpl)(nil)

// NewPipeline 创建 PipelineImpl，并确保 rag_docs 表存在。
func NewPipeline(db *sql.DB) *PipelineImpl {
	// CREATE TABLE IF NOT EXISTS，幂等；上线前阶段可随主 schema 建表
	_, _ = db.Exec(`
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
	return &PipelineImpl{db: db}
}

// Ingest 将文档转换为 Chunk 并持久化。
func (p *PipelineImpl) Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error) {
	if doc == nil {
		return nil, perrors.New(perrors.CodeInvalidInput, "knowledge: doc is nil")
	}
	content := string(doc.Raw)

	chunker := &DefaultChunker{}
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
			return nil, perrors.Wrap(perrors.CodeInternal, "ingester: insert chunk failed", err)
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
		return nil, perrors.Wrap(perrors.CodeInternal, "ingester: upsert rag_docs failed", docErr)
	}

	return tree, nil
}

// Delete 删除指定文档的所有片段。
func (p *PipelineImpl) Delete(ctx context.Context, uri string) error {
	_, err := p.db.ExecContext(ctx, "DELETE FROM rag_chunks WHERE doc_id = ?", uri)
	return err
}
