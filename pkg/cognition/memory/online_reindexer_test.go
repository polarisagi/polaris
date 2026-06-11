package memory

import (
	"context"
	"database/sql"
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
			event_uuid          TEXT    NOT NULL DEFAULT ''
		)
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func insertEvent(t *testing.T, db *sql.DB, content, version string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO episodic_events (content, embed_model_version) VALUES (?, ?)`,
		content, version,
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

	insertEvent(t, db, "hello world", "")    // 未索引
	insertEvent(t, db, "go is great", "")    // 未索引
	insertEvent(t, db, "already done", "v2") // 已是最新版本

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

	insertEvent(t, db, "current", "v3") // 已是最新

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
