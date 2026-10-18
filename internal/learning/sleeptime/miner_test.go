package sleeptime_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/polarisagi/polaris/internal/learning/sleeptime"
)

func TestMineFailures(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite: %v", err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE IF NOT EXISTS llm_calls (
		id               TEXT PRIMARY KEY,
		created_at       INTEGER NOT NULL,
		session_id       TEXT    NOT NULL DEFAULT '',
		purpose          TEXT    NOT NULL DEFAULT '',
		provider         TEXT    NOT NULL,
		model_id         TEXT    NOT NULL DEFAULT '',
		model_pool       TEXT    NOT NULL DEFAULT '',
		thinking_mode    TEXT    NOT NULL DEFAULT '',
		streaming        INTEGER NOT NULL DEFAULT 0,
		status           TEXT    NOT NULL,
		input_tokens     INTEGER NOT NULL DEFAULT 0,
		cache_hit_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens    INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		latency_ms       INTEGER NOT NULL DEFAULT 0,
		cost_usd         REAL    NOT NULL DEFAULT 0,
		error            TEXT    NOT NULL DEFAULT ''
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}

	now := time.Now().UnixMilli()
	rows := []struct {
		id      string
		purpose string
		status  string
		errMsg  string
	}{
		{"1", "agentic_loop", "error", "context deadline exceeded"},
		{"2", "agentic_loop", "error", "connection timeout after 30s"},
		{"3", "plan", "error", "unexpected EOF in json unmarshal"},
		{"4", "plan", "error", "schema validation error: missing field"},
		{"5", "execute_tool:bash", "error", "exit status 1: file not found"},
		{"6", "agentic_loop", "ok", ""}, // success, should be excluded
	}

	for _, r := range rows {
		_, err := db.Exec(`
			INSERT INTO llm_calls (id, created_at, session_id, purpose, provider, status, error)
			VALUES (?, ?, 'sess_1', ?, 'test_prov', ?, ?)
		`, r.id, now, r.purpose, r.status, r.errMsg)
		if err != nil {
			t.Fatalf("failed to insert test row: %v", err)
		}
	}

	weaknesses, err := sleeptime.MineFailures(context.Background(), db, 50)
	if err != nil {
		t.Fatalf("MineFailures failed: %v", err)
	}

	if len(weaknesses) == 0 {
		t.Fatalf("expected weaknesses, got 0")
	}

	// 验证聚类聚合
	foundTimeout := false
	for _, w := range weaknesses {
		if w.Signature == "agentic_loop:timeout" {
			foundTimeout = true
			if w.Count != 2 {
				t.Errorf("expected count 2 for agentic_loop:timeout, got %d", w.Count)
			}
			if len(w.SampleErrors) != 2 {
				t.Errorf("expected 2 sample errors, got %d", len(w.SampleErrors))
			}
		}
	}
	if !foundTimeout {
		t.Errorf("expected cluster agentic_loop:timeout not found in %+v", weaknesses)
	}
}
