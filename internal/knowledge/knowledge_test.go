package knowledge

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	_ "modernc.org/sqlite"
)

// failingOutboxWriter 的 Write 总是失败，用于验证阶段02修复：GraphBuild outbox
// 投递失败必须向上返回错误，不得静默吞没（GR-7-003）。
type failingOutboxWriter struct{}

func (f *failingOutboxWriter) Write(ctx context.Context, entry protocol.OutboxEntry) error {
	return apperr.New(apperr.CodeInternal, "simulated outbox write failure")
}

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	// modernc.org/sqlite 的 ":memory:" 库是"每连接独立"的：database/sql 默认
	// 允许连接池开多条连接，若测试并发/重试触发了第二条连接，它会拿到一个
	// 全新的空库，看不到下面建的表，报 "no such table: rag_chunks"——本次
	// 复核跑 -count=3 -race 时实测复现（约 1/3 概率）。限制为单连接消除该
	// 竞态，语义上等价于其他测试用 file::memory:?cache=shared 的目的。
	db.SetMaxOpenConns(1)

	// Create rag_chunks schema（含 031_rag_lineage 新增的 lineage 字段）
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS rag_chunks (
			id                   TEXT PRIMARY KEY,
			doc_id               TEXT NOT NULL,
			content              TEXT NOT NULL,
			taint_level          INTEGER NOT NULL DEFAULT 1,
			taint_source         TEXT,
			taint_hmac           TEXT NOT NULL DEFAULT '',
			source_uri           TEXT NOT NULL DEFAULT '',
			doc_version          TEXT NOT NULL DEFAULT '',
			chunk_seq            INTEGER NOT NULL DEFAULT 0,
			content_hash         TEXT NOT NULL DEFAULT '',
			embed_model_version  TEXT NOT NULL DEFAULT '',
			created_at           TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			deleted_at           INTEGER
		);

		CREATE VIRTUAL TABLE IF NOT EXISTS rag_chunks_fts USING fts5(
			content,
			content='rag_chunks',
			content_rowid='rowid'
		);

		CREATE TRIGGER IF NOT EXISTS rag_chunks_ai AFTER INSERT ON rag_chunks BEGIN
		  INSERT INTO rag_chunks_fts(rowid, content) VALUES (new.rowid, new.content);
		END;
	`)
	if err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}

	return db
}

func TestPipelineImpl_Ingest(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := NewPipeline(db, nil, nil, nil, nil)

	doc := &Document{
		Ref: DocumentRef{
			URI:         "doc1",
			Title:       "Test Document",
			ContentHash: "hash123",
		},
		Raw: []byte("Paragraph 1\n\nParagraph 2\n\nParagraph 3"),
	}

	tree, err := pipeline.Ingest(context.Background(), doc, TaintLow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tree == nil {
		t.Fatal("expected non-nil DocTree")
	}
	if len(tree.Document.Children) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(tree.Document.Children))
	}

	// Verify storage
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM rag_chunks").Scan(&count)
	if err != nil {
		t.Fatalf("failed to count chunks: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 chunks in db, got %d", count)
	}
}

// TestPipelineImpl_Ingest_OutboxWriteFailure_ReturnsError_S02 验证阶段02修复：
// GraphBuild outbox 投递失败必须向上返回错误。回归锚点：修复前
// `_ = p.outboxWriter.Write(ctx, ev)` 吞没该错误，调用方拿到 nil error + 非 nil
// tree，会误以为摄入完全成功，但知识图谱构建这条链路从此对该文档永久不会触发。
func TestPipelineImpl_Ingest_OutboxWriteFailure_ReturnsError_S02(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := NewPipeline(db, nil, &failingOutboxWriter{}, nil, nil)

	doc := &Document{
		Ref: DocumentRef{URI: "doc-outbox-fail", Title: "Test", ContentHash: "hash-outbox-fail"},
		Raw: []byte("Paragraph 1\n\nParagraph 2"),
	}

	tree, err := pipeline.Ingest(context.Background(), doc, TaintLow)
	if err == nil {
		t.Fatal("expected error when outbox write fails, got nil")
	}
	// chunks 已经落盘（先于 outbox 投递），tree 仍应非 nil 供调用方按需处理，
	// 但必须同时拿到 error 信号，不能误判为完全成功。
	if tree == nil {
		t.Error("expected non-nil tree despite outbox failure (chunks already committed)")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM rag_chunks WHERE doc_id = ?", doc.Ref.URI).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count == 0 {
		t.Error("expected chunks to remain committed despite outbox failure")
	}
}

// TestPipelineImpl_GetRecentChunks 2026-07-14 回归防护：此前硬编码返回同一条
// 写死字符串，忽略真实 rag_chunks 内容与 limit 参数。验证改为真查 DB 后能
// 正确读取最近写入的内容、遵守 limit、跳过软删除行、且不返回自身写死的占位句。
func TestPipelineImpl_GetRecentChunks(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := NewPipeline(db, nil, nil, nil, nil)

	doc := &Document{
		Ref: DocumentRef{URI: "doc1", Title: "Test", ContentHash: "hash1"},
		Raw: []byte("Alpha chunk\n\nBeta chunk\n\nGamma chunk"),
	}
	if _, err := pipeline.Ingest(context.Background(), doc, TaintLow); err != nil {
		t.Fatalf("ingest failed: %v", err)
	}

	chunks, err := pipeline.GetRecentChunks(context.Background(), 2)
	if err != nil {
		t.Fatalf("GetRecentChunks failed: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected limit=2 to be respected, got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if strings.Contains(c, "mocked recent chunk") {
			t.Errorf("GetRecentChunks must not return the old hardcoded placeholder, got %q", c)
		}
	}

	// 软删除后不应再被返回。
	if _, err := db.Exec("UPDATE rag_chunks SET deleted_at = 1"); err != nil {
		t.Fatalf("failed to soft-delete chunks: %v", err)
	}
	chunks, err = pipeline.GetRecentChunks(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetRecentChunks after soft-delete failed: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks after soft-delete, got %d", len(chunks))
	}
}

func TestHybridRetrieverImpl_Search(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := NewPipeline(db, nil, nil, nil, nil)
	retriever := NewHybridRetrieverWithCognitive(db, nil, nil, 0)

	doc := &Document{
		Ref: DocumentRef{
			URI:         "doc1",
			ContentHash: "hash123",
		},
		Raw: []byte("Apples are red\n\nBananas are yellow\n\nGrapes are green"),
	}
	_, err := pipeline.Ingest(context.Background(), doc, TaintNone)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	results, err := retriever.Search(context.Background(), "yellow", types.SearchScope{}, types.RetrievalConfig{FinalTopK: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0].Content, "Bananas") {
		t.Fatalf("expected chunk with Bananas, got %s", results[0].Content)
	}
}
