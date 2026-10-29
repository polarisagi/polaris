package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

type fakeChunkEmbedder struct {
	version string
	failAll bool
	failIDs map[string]bool // 按内容匹配失败
	calls   int
}

func (f *fakeChunkEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls++
	if f.failAll || f.failIDs[text] {
		return nil, errors.New("embed boom")
	}
	return []float32{1, 2, 3}, nil
}

func (f *fakeChunkEmbedder) ModelVersion() string { return f.version }

type fakeVecIndex struct {
	upserts map[string]int
	err     error
}

func (f *fakeVecIndex) VecUpsert(id string, _ []float32) error {
	if f.err != nil {
		return f.err
	}
	if f.upserts == nil {
		f.upserts = map[string]int{}
	}
	f.upserts[id]++
	return nil
}

func insertRagChunk(t *testing.T, db *sql.DB, id, content, ver string, deleted bool) {
	t.Helper()
	var del any
	if deleted {
		del = "2026-01-01T00:00:00Z"
	}
	if _, err := db.Exec(
		`INSERT INTO rag_chunks (id, doc_id, content, embed_model_version, deleted_at) VALUES (?, 'd', ?, ?, ?)`,
		id, content, ver, del); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
}

func chunkVersion(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var v string
	if err := db.QueryRow(`SELECT embed_model_version FROM rag_chunks WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("query version: %v", err)
	}
	return v
}

func TestKnowledgeReindexer_SkipsWhenEngineNotReady(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	insertRagChunk(t, db, "c1", "a", "", false)
	emb := &fakeChunkEmbedder{version: ""}
	idx := &fakeVecIndex{}
	n, err := NewKnowledgeReindexer(db, emb, idx).RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("want (0,nil), got (%d,%v)", n, err)
	}
	if emb.calls != 0 || len(idx.upserts) != 0 {
		t.Errorf("must not embed when not ready: calls=%d upserts=%v", emb.calls, idx.upserts)
	}
}

func TestKnowledgeReindexer_FirstIndexing(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	insertRagChunk(t, db, "c1", "a", "", false)
	insertRagChunk(t, db, "c2", "b", "", false)
	emb := &fakeChunkEmbedder{version: "m@3"}
	idx := &fakeVecIndex{}
	n, err := NewKnowledgeReindexer(db, emb, idx).RunOnce(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("want (2,nil), got (%d,%v)", n, err)
	}
	for _, id := range []string{"c1", "c2"} {
		if v := chunkVersion(t, db, id); v != "m@3" {
			t.Errorf("%s version = %q, want m@3", id, v)
		}
		if idx.upserts[id] != 1 {
			t.Errorf("%s upserts = %d, want 1", id, idx.upserts[id])
		}
	}
	// 幂等：已是当前版本的不再处理。
	n, err = NewKnowledgeReindexer(db, emb, idx).RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("second run want (0,nil), got (%d,%v)", n, err)
	}
}

func TestKnowledgeReindexer_ReembedOnVersionSwitch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	insertRagChunk(t, db, "c1", "a", "old@3", false)
	emb := &fakeChunkEmbedder{version: "new@3"}
	idx := &fakeVecIndex{}
	n, err := NewKnowledgeReindexer(db, emb, idx).RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("want (1,nil), got (%d,%v)", n, err)
	}
	if v := chunkVersion(t, db, "c1"); v != "new@3" {
		t.Errorf("version = %q, want new@3", v)
	}
}

func TestKnowledgeReindexer_EmbedFailureKeepsOldVersion(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	insertRagChunk(t, db, "c1", "bad", "old@3", false)
	insertRagChunk(t, db, "c2", "good", "old@3", false)
	emb := &fakeChunkEmbedder{version: "new@3", failIDs: map[string]bool{"bad": true}}
	idx := &fakeVecIndex{}
	n, err := NewKnowledgeReindexer(db, emb, idx).RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("want (1,nil), got (%d,%v)", n, err)
	}
	if v := chunkVersion(t, db, "c1"); v != "old@3" {
		t.Errorf("failed chunk version = %q, want old@3 (retry next round)", v)
	}
	if v := chunkVersion(t, db, "c2"); v != "new@3" {
		t.Errorf("good chunk version = %q, want new@3", v)
	}
}

func TestKnowledgeReindexer_VecUpsertFailureKeepsVersion(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	insertRagChunk(t, db, "c1", "a", "", false)
	idx := &fakeVecIndex{err: errors.New("surreal down")}
	n, err := NewKnowledgeReindexer(db, &fakeChunkEmbedder{version: "m@3"}, idx).RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("want (0,nil), got (%d,%v)", n, err)
	}
	if v := chunkVersion(t, db, "c1"); v != "" {
		t.Errorf("version = %q, want '' (not stamped without vector)", v)
	}
}

func TestKnowledgeReindexer_IgnoresDeletedRows(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	insertRagChunk(t, db, "c1", "a", "", true)
	emb := &fakeChunkEmbedder{version: "m@3"}
	n, err := NewKnowledgeReindexer(db, emb, &fakeVecIndex{}).RunOnce(context.Background())
	if err != nil || n != 0 || emb.calls != 0 {
		t.Fatalf("want (0,nil) and no embed calls, got (%d,%v) calls=%d", n, err, emb.calls)
	}
}

func TestKnowledgeReindexer_AbortsAfterConsecutiveFailures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	for _, id := range []string{"c1", "c2", "c3", "c4", "c5"} {
		insertRagChunk(t, db, id, id, "", false)
	}
	emb := &fakeChunkEmbedder{version: "m@3", failAll: true}
	n, err := NewKnowledgeReindexer(db, emb, &fakeVecIndex{}).RunOnce(context.Background())
	if err == nil || n != 0 {
		t.Fatalf("want abort error, got (%d,%v)", n, err)
	}
	if emb.calls != knowledgeReindexMaxConsecutiveEmbedFails {
		t.Errorf("embed calls = %d, want %d (abort at threshold)", emb.calls, knowledgeReindexMaxConsecutiveEmbedFails)
	}
}

// ADR-0113 决策七：队首永久失败的毒行（数量 >= 批大小）不得饿死其后的正常行——同一轮内游标推进。
func TestKnowledgeReindexer_DrainCursorSkipsPoisonRows(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	insertRagChunk(t, db, "p1", "poison1", "", false)
	insertRagChunk(t, db, "p2", "poison2", "", false)
	insertRagChunk(t, db, "g1", "good1", "", false)
	insertRagChunk(t, db, "g2", "good2", "", false)
	insertRagChunk(t, db, "g3", "good3", "", false)
	emb := &fakeChunkEmbedder{version: "m@3", failIDs: map[string]bool{"poison1": true, "poison2": true}}
	r := NewKnowledgeReindexer(db, emb, &fakeVecIndex{})
	r.batchSize = 2 // 首批恰为两条毒行：旧实现 processed==0 即退出，g* 永不被索引
	if err := r.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, id := range []string{"g1", "g2", "g3"} {
		if v := chunkVersion(t, db, id); v != "m@3" {
			t.Errorf("%s version = %q, want m@3 (indexed in same round)", id, v)
		}
	}
	for _, id := range []string{"p1", "p2"} {
		if v := chunkVersion(t, db, id); v != "" {
			t.Errorf("%s version = %q, want '' (retry next round)", id, v)
		}
	}
}

// drain 遇到连续嵌入失败仍返回中止错误，不被游标吞掉。
func TestKnowledgeReindexer_DrainAbortsOnConsecutiveFailures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		insertRagChunk(t, db, id, id, "", false)
	}
	r := NewKnowledgeReindexer(db, &fakeChunkEmbedder{version: "m@3", failAll: true}, &fakeVecIndex{})
	if err := r.drain(context.Background()); err == nil {
		t.Fatal("want abort error")
	}
}

func TestKnowledgeReindexer_KickAndLoopExit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	emb := &fakeChunkEmbedder{version: "m@3"}
	r := NewKnowledgeReindexer(db, emb, &fakeVecIndex{})
	r.Kick()
	r.Kick() // 容量 1，第二次不得阻塞
	var nilR *KnowledgeReindexer
	nilR.Kick() // nil 安全

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Loop(ctx, time.Hour); close(done) }()

	insertRagChunk(t, db, "late", "x", "", false)
	r.Kick()
	deadline := time.After(3 * time.Second)
	for chunkVersion(t, db, "late") != "m@3" {
		select {
		case <-deadline:
			t.Fatal("Kick did not trigger indexing")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Loop did not exit on ctx cancel")
	}
}

func TestIngest_KicksReindexerAfterCommit_WritesPendingVersion(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	kicks := 0
	pipeline := newTestIngestionPipeline(db, nil).WithReindexKick(func() { kicks++ })
	doc := &Document{
		Ref: DocumentRef{URI: "kick-doc", Title: "T", ContentHash: "h-kick"},
		Raw: []byte("Paragraph 1\n\nParagraph 2"),
	}
	if _, err := pipeline.Ingest(context.Background(), doc, TaintLow); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if kicks != 1 {
		t.Errorf("kicks = %d, want 1 (after successful commit)", kicks)
	}
	var notPending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rag_chunks WHERE embed_model_version != ''`).Scan(&notPending); err != nil {
		t.Fatalf("count: %v", err)
	}
	if notPending != 0 {
		t.Errorf("ingest must write '' (pending) version, found %d non-empty", notPending)
	}
}
