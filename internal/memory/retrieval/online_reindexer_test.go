package retrieval

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

// stubEmbedder returns a fixed-length vector and a fixed model version.
type stubEmbedder struct {
	version string
	dim     int
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	vec := make([]float32, s.dim)
	for i := range vec {
		vec[i] = float32(i) * 0.001
	}
	return vec, nil
}

func (s *stubEmbedder) ModelVersion() string { return s.version }

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		CREATE TABLE episodic_events (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id          TEXT    NOT NULL DEFAULT '',
			seq                 INTEGER NOT NULL DEFAULT 0,
			timestamp           INTEGER NOT NULL DEFAULT 0,
			event_type          TEXT    NOT NULL DEFAULT '',
			source              TEXT    NOT NULL DEFAULT '',
			content             TEXT    NOT NULL DEFAULT '',
			embedding           BLOB,
			salience            REAL    NOT NULL DEFAULT 0.5,
			decay_weight        REAL    NOT NULL DEFAULT 1.0,
			occurred_at         INTEGER,
			embed_model_version TEXT    NOT NULL DEFAULT '',
			event_uuid          TEXT    NOT NULL DEFAULT '',
			archived            INTEGER NOT NULL DEFAULT 0
		)
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func insertEvent(t *testing.T, db *sql.DB, content, version string, archived int) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO episodic_events (content, embed_model_version, archived) VALUES (?, ?, ?)`,
		content, version, archived,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestOnlineReindexer_IndexesUnindexedRows(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	embedder := &stubEmbedder{version: "v2", dim: 4}
	r := NewOnlineReindexer(db, embedder)

	insertEvent(t, db, "hello world", "", 0)    // 未索引
	insertEvent(t, db, "go is great", "", 0)    // 未索引
	insertEvent(t, db, "already done", "v2", 0) // 已是最新版本

	processed, remaining, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if processed != 2 {
		t.Errorf("processed=%d, want 2", processed)
	}
	if remaining {
		t.Error("remaining should be false after indexing all unindexed rows")
	}

	// 验证 embed_model_version 已更新
	var ver string
	if err := db.QueryRow(`SELECT embed_model_version FROM episodic_events WHERE id = 1`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != "v2" {
		t.Errorf("embed_model_version=%q, want %q", ver, "v2")
	}

	// 验证 embedding BLOB 已写入（长度 = dim * 2 bytes for float16）
	var blob []byte
	if err := db.QueryRow(`SELECT embedding FROM episodic_events WHERE id = 1`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if want := embedder.dim * 2; len(blob) != want {
		t.Errorf("embedding blob len=%d, want %d", len(blob), want)
	}
}

func TestOnlineReindexer_SkipsCurrentVersion(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	embedder := &stubEmbedder{version: "v3", dim: 4}
	r := NewOnlineReindexer(db, embedder)

	insertEvent(t, db, "current", "v3", 0) // 已是最新

	processed, remaining, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if processed != 0 {
		t.Errorf("processed=%d, want 0 (all rows already current)", processed)
	}
	if remaining {
		t.Error("remaining should be false when all rows are up to date")
	}
}

func TestOnlineReindexer_SkipsColdEvents(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	embedder := &stubEmbedder{version: "v4", dim: 4}
	r := NewOnlineReindexer(db, embedder)

	insertEvent(t, db, "cold event", "", 1) // archived=1, 应该被跳过
	insertEvent(t, db, "hot event", "", 0)  // archived=0, 正常填充

	processed, remaining, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed=%d, want 1", processed)
	}
	if remaining {
		t.Error("remaining should be false")
	}

	// 验证 hot event 已更新
	var ver string
	if err := db.QueryRow(`SELECT embed_model_version FROM episodic_events WHERE content = 'hot event'`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != "v4" {
		t.Errorf("hot event embed_model_version=%q, want %q", ver, "v4")
	}

	// 验证 cold event 未更新
	var coldVer string
	if err := db.QueryRow(`SELECT embed_model_version FROM episodic_events WHERE content = 'cold event'`).Scan(&coldVer); err != nil {
		t.Fatal(err)
	}
	if coldVer != "" {
		t.Errorf("cold event embed_model_version=%q, want empty string", coldVer)
	}
}

func TestOnlineReindexer_EmptyTable(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	embedder := &stubEmbedder{version: "v1", dim: 8}
	r := NewOnlineReindexer(db, embedder)

	processed, remaining, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if processed != 0 || remaining {
		t.Errorf("got processed=%d remaining=%v, want 0 false", processed, remaining)
	}
}

func TestF32toF16_Roundtrip(t *testing.T) {
	cases := []struct {
		in   float32
		desc string
	}{
		{0.0, "zero"},
		{1.0, "one"},
		{-1.0, "negative one"},
		{0.5, "half"},
	}
	for _, c := range cases {
		h := f32tof16(c.in)
		// 验证符号位一致性（正负不变）
		if c.in < 0 && (h>>15) != 1 {
			t.Errorf("%s: sign bit wrong for %v", c.desc, c.in)
		}
		if c.in > 0 && (h>>15) != 0 {
			t.Errorf("%s: sign bit wrong for %v", c.desc, c.in)
		}
	}
}

func TestEncodeFloat16_Length(t *testing.T) {
	vec := make([]float32, 10)
	blob := encodeFloat16(vec)
	if len(blob) != 20 {
		t.Errorf("blob len=%d, want 20 (10 float16 * 2 bytes)", len(blob))
	}
}

// failingEmbedder 每次都失败，记录调用次数。
type failingEmbedder struct{ calls int }

func (f *failingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	return nil, errors.New("embed backend down")
}

func (f *failingEmbedder) ModelVersion() string { return "v-fail" }

// ADR-0109 D5：嵌入后端整体不可用时，Run 必须在连续失败上限处中止并返回错误，
// 让调用方的退避器生效；此前逐条吞错使整批每条都跑到超时，退避器永远看不到失败。
func TestOnlineReindexer_AbortsOnConsecutiveEmbedFailures(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	for i := 0; i < 10; i++ {
		insertEvent(t, db, "row", "", 0)
	}
	emb := &failingEmbedder{}
	r := NewOnlineReindexer(db, emb)

	processed, remaining, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when embedder fails consecutively")
	}
	if emb.calls != reindexMaxConsecutiveEmbedFails {
		t.Errorf("embed calls=%d, want %d (batch must abort early)", emb.calls, reindexMaxConsecutiveEmbedFails)
	}
	if processed != 0 || !remaining {
		t.Errorf("processed=%d remaining=%v, want 0/true", processed, remaining)
	}
}

// flakyEmbedder 交替失败/成功：单条失败不应中止整批。
type flakyEmbedder struct{ calls int }

func (f *flakyEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	if f.calls%2 == 1 {
		return nil, errors.New("transient")
	}
	return []float32{0.1, 0.2}, nil
}

func (f *flakyEmbedder) ModelVersion() string { return "v-flaky" }

func TestOnlineReindexer_IsolatedFailuresDoNotAbort(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	for i := 0; i < 6; i++ {
		insertEvent(t, db, "row", "", 0)
	}
	emb := &flakyEmbedder{}
	r := NewOnlineReindexer(db, emb)

	processed, _, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("isolated failures must not abort the batch: %v", err)
	}
	if emb.calls != 6 || processed != 3 {
		t.Errorf("calls=%d processed=%d, want 6/3", emb.calls, processed)
	}
}
