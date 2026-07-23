package graphrag

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// setupSemanticDB 创建 in-memory semantic_entities 表供测试使用。
func setupSemanticDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS semantic_entities (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		entity_type TEXT NOT NULL,
		name TEXT NOT NULL,
		properties TEXT,
		embedding BLOB,
		version INTEGER DEFAULT 0,
		source_type TEXT DEFAULT 'manual',
		status TEXT DEFAULT 'active',
		created_at INTEGER,
		updated_at INTEGER
	)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// sqlQuerier 将 *sql.DB 适配为 protocol.SQLQuerier 的最小包装。
type testQuerier struct{ db *sql.DB }

func (q *testQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}
func (q *testQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}
func (q *testQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

// TestGraphWriter_UpsertEntity_DeduplicatesGraphragIngest 验证：
// 1. 首次 UpsertEntity 写入 source_type='graphrag_ingest'
// 2. 同名同类型高相似度低版本再次 upsert 时不产生重复行（B2 验收）。
func TestGraphWriter_UpsertEntity_DeduplicatesGraphragIngest(t *testing.T) {
	db := setupSemanticDB(t)
	defer db.Close()

	gw := &GraphWriter{
		bus:        nil, // bus 为 nil，Submit 会 panic，但 semanticDB 路径在 Submit 之前
		fetcher:    nil,
		semanticDB: &testQuerier{db: db},
	}

	ctx := context.Background()

	// embedding: 单位向量
	emb := []float32{1.0, 0.0, 0.0}

	e1 := &Entity{
		ID:          "e-test-1",
		Name:        "Polaris",
		Type:        "Project",
		SyncVersion: 1,
		Embedding:   emb,
	}

	// 手动触发 semanticDB 路径（绕过 bus.Submit 会 nil panic：只测 semanticDB 分支）
	// 调用 upsertToSemanticDB 内部逻辑：先 query，再 insert/update。
	// 由于 UpsertEntity 最终会调用 bus.Submit（nil panic），我们直接测 semanticDB 分支的效果。
	// 方法：先手动 insert，再调用 UpsertEntity 确认不产生重复行。

	// 先直接插入一条 source_type='graphrag_ingest' 的记录
	_, err := db.Exec(`INSERT INTO semantic_entities (entity_type, name, properties, embedding, version, source_type, created_at, updated_at)
		VALUES (?, ?, '{}', ?, ?, 'graphrag_ingest', 0, 0)`,
		e1.Type, e1.Name, float32sToBytes(emb), e1.SyncVersion)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 查行数，应为 1
	countRows := func() int {
		var n int
		_ = db.QueryRow("SELECT COUNT(*) FROM semantic_entities WHERE name=? AND entity_type=?", e1.Name, e1.Type).Scan(&n)
		return n
	}
	if got := countRows(); got != 1 {
		t.Fatalf("before dedup: expected 1 row, got %d", got)
	}

	// 构造低版本高相似度实体，模拟重复写入（sem=1.0，version 更低）
	e2 := &Entity{
		ID:          "e-test-2",
		Name:        "Polaris", // 同名
		Type:        "Project", // 同类型
		SyncVersion: 0,         // 低于已存在的 version=1
		Embedding:   emb,       // 完全相同 embedding → 相似度 1.0 > 0.95
	}

	// 只测 semanticDB 去重逻辑（不进 bus.Submit）
	gw.upsertToSemanticDB(ctx, e2)

	// 行数仍应为 1，不产生重复
	if got := countRows(); got != 1 {
		t.Errorf("after dedup: expected 1 row (no duplicate), got %d", got)
	}
}

// TestGraphWriter_UpsertEntity_InsertsNewEntity 验证不存在的实体被插入且 source_type='graphrag_ingest'。
func TestGraphWriter_UpsertEntity_InsertsNewEntity(t *testing.T) {
	db := setupSemanticDB(t)
	defer db.Close()

	gw := &GraphWriter{
		bus:        nil,
		fetcher:    nil,
		semanticDB: &testQuerier{db: db},
	}

	ctx := context.Background()
	emb := []float32{0.0, 1.0, 0.0}

	e := &Entity{
		ID:          "e-new-1",
		Name:        "PolarisRAG",
		Type:        "Component",
		SyncVersion: 3,
		Embedding:   emb,
	}

	gw.upsertToSemanticDB(ctx, e)

	var sourceType string
	err := db.QueryRow("SELECT source_type FROM semantic_entities WHERE name=? AND entity_type=?", e.Name, e.Type).Scan(&sourceType)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if sourceType != "graphrag_ingest" {
		t.Errorf("expected source_type='graphrag_ingest', got %q", sourceType)
	}
}
