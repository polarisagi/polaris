package knowledge

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}

	// Create rag_chunks schema（含 031_rag_lineage 新增的 lineage 字段）
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS rag_chunks (
			id                   TEXT PRIMARY KEY,
			doc_id               TEXT NOT NULL,
			content              TEXT NOT NULL,
			taint_level          INTEGER NOT NULL DEFAULT 1,
			taint_source         TEXT,
			source_uri           TEXT NOT NULL DEFAULT '',
			doc_version          TEXT NOT NULL DEFAULT '',
			chunk_seq            INTEGER NOT NULL DEFAULT 0,
			content_hash         TEXT NOT NULL DEFAULT '',
			embed_model_version  TEXT NOT NULL DEFAULT '',
			created_at           TIMESTAMP DEFAULT CURRENT_TIMESTAMP
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
	pipeline := NewPipeline(db)

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

func TestHybridRetrieverImpl_Search(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := NewPipeline(db)
	retriever := NewHybridRetriever(db)

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

	results, err := retriever.Search(context.Background(), &SearchQuery{
		Text: "yellow",
		TopK: 10,
	})
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
