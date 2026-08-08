package knowledge

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store"
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

// applyRAGSchema 从 DDL SSoT（internal/protocol/schema/009_rag_chunks.sql）建表，
// 不再手抄一份列集。
//
// 2026-08-08 结构性修复：本函数原先内联手写 rag_chunks 建表语句且完全不建
// rag_docs，于是被测代码自己的 CREATE TABLE IF NOT EXISTS 兜了底，测试跑在一份
// 只存在于测试进程里的表结构上。真实后果是 rag_docs 长期存在三套互不兼容的列
// 集（SSoT / ingester.go / 本文件），生产两条摄取路径实测双双报
// "no such column"，而 CI 全绿。用 SSoT 建表后，任何列漂移会在测试里直接暴露。
func applyRAGSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ddl, err := schema.FS.ReadFile("009_rag_chunks.sql")
	if err != nil {
		t.Fatalf("read DDL SSoT 009_rag_chunks.sql: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply DDL SSoT: %v", err)
	}
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
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
	applyRAGSchema(t, db)
	return db
}

// newTestIngestionPipeline 按生产装配方式构造摄取流水线：走 StorageRouter，
// 与 cmd/polaris/boot_knowledge.go 的 NewDefaultIngestionPipeline 同一条路径。
func newTestIngestionPipeline(db *sql.DB, outbox protocol.OutboxWriter) *DefaultIngestionPipeline {
	return NewDefaultIngestionPipeline(store.NewStorageRouter(&dbOnlyStore{db: db}, nil), nil, outbox, nil, nil)
}

func TestIngestionPipeline_Ingest(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := newTestIngestionPipeline(db, nil)

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

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM rag_chunks").Scan(&count); err != nil {
		t.Fatalf("failed to count chunks: %v", err)
	}
	if count == 0 {
		t.Fatal("expected chunks to be persisted")
	}
}

// TestIngestionPipeline_Ingest_WritesContentHash 回归锚点（2026-08-08）：
// rag_docs.content_hash 是增量摄取的唯一判据。此前 INSERT 根本不写这一列，
// 且 SSoT 里也没有这一列，checkIngestCache 每次读都报 "no such column:
// content_hash" → 缓存 100% 落空 → 每次同步全量重算 embedding。
func TestIngestionPipeline_Ingest_WritesContentHash(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := newTestIngestionPipeline(db, nil)

	doc := &Document{
		Ref: DocumentRef{URI: "doc-hash", Title: "T", ContentHash: "hash-abc"},
		Raw: []byte("Alpha\n\nBeta"),
	}
	if _, err := pipeline.Ingest(context.Background(), doc, TaintLow); err != nil {
		t.Fatalf("ingest failed: %v", err)
	}

	var got string
	if err := db.QueryRow("SELECT content_hash FROM rag_docs WHERE uri = ?", doc.Ref.URI).Scan(&got); err != nil {
		t.Fatalf("read back content_hash: %v", err)
	}
	if got != "hash-abc" {
		t.Fatalf("expected content_hash %q persisted, got %q", "hash-abc", got)
	}

	// 二次摄取同一 hash 必须命中缓存：chunk 数不增长。
	var before int
	if err := db.QueryRow("SELECT COUNT(*) FROM rag_chunks").Scan(&before); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if _, err := pipeline.Ingest(context.Background(), doc, TaintLow); err != nil {
		t.Fatalf("second ingest failed: %v", err)
	}
	var after int
	if err := db.QueryRow("SELECT COUNT(*) FROM rag_chunks").Scan(&after); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if after != before {
		t.Fatalf("expected incremental cache hit (chunk count unchanged), got %d → %d", before, after)
	}
}

// TestIngestionPipeline_Ingest_OutboxWriteFailure_ReturnsError_S02 验证阶段02
// 修复：outbox 投递失败必须向上返回错误。回归锚点：修复前 `_ = Write(...)`
// 吞没该错误，调用方拿到 nil error + 非 nil tree，会误以为摄入完全成功，但
// 知识图谱构建这条链路从此对该文档永久不会触发。
func TestIngestionPipeline_Ingest_OutboxWriteFailure_ReturnsError_S02(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := newTestIngestionPipeline(db, &failingOutboxWriter{})

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
		t.Fatal("expected non-nil tree despite outbox failure (chunks already committed)")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM rag_chunks").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count == 0 {
		t.Error("expected chunks to remain committed despite outbox failure")
	}
}

// TestIngestionPipeline_GetRecentChunks 回归防护：GetRecentChunks 必须真查 DB。
// 该缺陷 2026-07-14 在 PipelineImpl 上修过一次，但生产调的是本类型
// （boot_agent.go 的 kb.Ingester），修复落在了孪生死实现上从未生效；孪生实现
// 已于 2026-08-08 删除，本用例锚定在唯一存活的实现上。
func TestIngestionPipeline_GetRecentChunks(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	pipeline := newTestIngestionPipeline(db, nil)

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
	pipeline := newTestIngestionPipeline(db, nil)
	retriever := NewHybridRetrieverWithCognitive(db, nil, nil, 0)

	doc := &Document{
		Ref: DocumentRef{
			URI:         "doc1",
			ContentHash: "hash123",
		},
		Raw: []byte("Apples are red\n\nBananas are yellow\n\nGrapes are green"),
	}
	if _, err := pipeline.Ingest(context.Background(), doc, TaintNone); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	results, err := retriever.Search(context.Background(), "yellow", types.SearchScope{}, types.RetrievalConfig{FinalTopK: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 不断言结果条数：DefaultIngestionPipeline 同时落 parent（整段）与 leaf（句级）
	// 两级分块，"yellow" 会同时命中两者，条数是分块粒度的函数而非检索正确性的
	// 判据。锚点收在"检索必须找到含 Bananas 的那一块"。
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	found := false
	for _, r := range results {
		if strings.Contains(r.Content, "Bananas") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a chunk containing Bananas, got %+v", results)
	}
}
