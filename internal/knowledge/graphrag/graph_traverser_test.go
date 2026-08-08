package graphrag

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTraverserTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	// 建表
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS semantic_entities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entity_type TEXT NOT NULL,
			name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			version INTEGER DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(entity_type, name)
		);
		CREATE TABLE IF NOT EXISTS semantic_relations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER REFERENCES semantic_entities(id),
			target_id INTEGER REFERENCES semantic_entities(id),
			relation_type TEXT NOT NULL,
			weight REAL DEFAULT 1.0,
			created_at INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active',
			valid_from INTEGER,
			valid_until INTEGER,
			superseded_by INTEGER REFERENCES semantic_relations(id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS uq_semantic_rel_active
			ON semantic_relations(source_id, target_id, relation_type) WHERE status = 'active';
		CREATE INDEX IF NOT EXISTS idx_semantic_rel_source ON semantic_relations(source_id);
		CREATE INDEX IF NOT EXISTS idx_semantic_rel_status ON semantic_relations(status);
		CREATE VIRTUAL TABLE IF NOT EXISTS rag_chunks_fts USING fts5(content, content=rag_chunks, content_rowid=rowid);
		CREATE TABLE IF NOT EXISTS rag_chunks (
			id TEXT PRIMARY KEY,
			doc_id TEXT,
			content TEXT,
			taint_level INTEGER DEFAULT 0,
			taint_source TEXT,
			deleted_at INTEGER
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	// 插入实体
	db.Exec(`INSERT INTO semantic_entities(entity_type, name, status, created_at, updated_at) VALUES ('Concept','GraphRAG','active',0,0)`)
	db.Exec(`INSERT INTO semantic_entities(entity_type, name, status, created_at, updated_at) VALUES ('Tool','BM25','active',0,0)`)
	db.Exec(`INSERT INTO semantic_relations(source_id, target_id, relation_type, created_at) VALUES (1,2,'USES',0)`)
	// 插入 chunk
	db.Exec(`INSERT INTO rag_chunks(id,doc_id,content,taint_level) VALUES ('c1','d1','GraphRAG uses BM25 for retrieval',0)`)
	db.Exec(`INSERT INTO rag_chunks_fts(rowid,content) VALUES (1,'GraphRAG uses BM25 for retrieval')`)
	return db
}

func TestGraphTraverser_TraverseChunks(t *testing.T) {
	db := setupTraverserTestDB(t)
	defer db.Close()
	gt := &GraphTraverser{db: db}
	chunks, err := gt.TraverseChunks(context.Background(), "GraphRAG", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk from graph traversal")
	}
}
