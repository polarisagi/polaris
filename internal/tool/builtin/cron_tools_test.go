package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE cron_jobs (
			id TEXT PRIMARY KEY,
			name TEXT,
			prompt TEXT,
			schedule TEXT,
			session_id TEXT,
			enabled INTEGER,
			last_run_at TEXT,
			next_run_at TEXT,
			failure_count INTEGER,
			circuit_open INTEGER,
			last_error TEXT,
			circuit_opened_at TEXT,
			created_at INTEGER DEFAULT (cast(strftime('%s', 'now') as int) * 1000)
		);
	`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	return db
}

func TestCronCreate(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	fn := makeCronCreateFn(repo.NewSQLiteCronRepository(db))
	ctx := context.Background()

	// Test invalid JSON
	_, err := fn(ctx, []byte(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid json")
	}

	// Test missing prompt
	_, err = fn(ctx, []byte(`{"schedule": "* * * * *"}`))
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}

	// Test missing schedule
	_, err = fn(ctx, []byte(`{"prompt": "do it"}`))
	if err == nil {
		t.Fatal("expected error for missing schedule")
	}

	// Test success
	out, err := fn(ctx, []byte(`{
		"name": "test job",
		"prompt": "do it",
		"schedule": "0 9 * * 1-5",
		"session_id": "session-123"
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}

	id, ok := res["id"].(string)
	if !ok || id == "" {
		t.Fatal("missing id in response")
	}
}

func TestCronList(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	_, _ = db.ExecContext(ctx, `
		INSERT INTO cron_jobs (id, name, prompt, schedule, session_id, enabled, last_run_at, next_run_at, failure_count, circuit_open, last_error, circuit_opened_at, created_at)
		VALUES ('cron_1', 'job 1', 'prompt 1', '* * * * *', '', 1, '', ?, 0, 0, '', '', ?)
	`, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))

	fn := makeCronListFn(repo.NewSQLiteCronRepository(db))

	out, err := fn(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res struct {
		Jobs  []map[string]any `json:"jobs"`
		Count int              `json:"count"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}

	if res.Count != 1 {
		t.Fatalf("expected count 1, got %d", res.Count)
	}
	if len(res.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(res.Jobs))
	}
	if res.Jobs[0]["id"] != "cron_1" {
		t.Fatalf("expected job id cron_1, got %v", res.Jobs[0]["id"])
	}
}

func TestCronDelete(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	_, _ = db.ExecContext(ctx, `
		INSERT INTO cron_jobs (id, name, prompt, schedule, session_id, enabled, last_run_at, next_run_at, failure_count, circuit_open, last_error, circuit_opened_at, created_at)
		VALUES ('cron_1', 'job 1', 'prompt 1', '* * * * *', '', 1, '', ?, 0, 0, '', '', ?)
	`, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))

	fn := makeCronDeleteFn(repo.NewSQLiteCronRepository(db))

	// Test invalid JSON
	_, err := fn(ctx, []byte(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid json")
	}

	// Test missing ID
	_, err = fn(ctx, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for missing id")
	}

	// Test non-existent ID
	_, err = fn(ctx, []byte(`{"id": "cron_999"}`))
	if err == nil {
		t.Fatal("expected error for non-existent id")
	}

	// Test success
	out, err := fn(ctx, []byte(`{"id": "cron_1"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if res["deleted"] != true {
		t.Fatalf("expected deleted=true")
	}

	// Verify it's actually deleted
	var count int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE id = 'cron_1'`).Scan(&count)
	if count != 0 {
		t.Fatal("job was not deleted from db")
	}
}
