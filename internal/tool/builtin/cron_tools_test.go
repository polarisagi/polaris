package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/observability/metrics"
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

	fn := MakeCronCreateFn(repo.NewSQLiteCronRepository(db))
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

// TestCronCreate_NextRunBackfillFailure_NonFatal_S02 验证阶段02修复：cron_create
// 创建任务后回填 next_run_at 失败（UpdateLastRun）不应导致整个工具调用失败——
// 任务本身已经通过 CreateCronJob 成功落库，只是调度时间可能与真实 cron 表达式
// 不一致（该后果已在代码注释中说明），必须 Warn+counter 而非静默或阻断。
func TestCronCreate_NextRunBackfillFailure_NonFatal_S02(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TRIGGER reject_next_run_update BEFORE UPDATE OF next_run_at ON cron_jobs
		BEGIN SELECT RAISE(ABORT, 'simulated next_run_at backfill failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	fn := MakeCronCreateFn(repo.NewSQLiteCronRepository(db))
	ctx := context.Background()

	before := metrics.GlobalCronNextRunWriteFailuresTotal.Load()

	out, err := fn(ctx, []byte(`{
		"name": "test job 2",
		"prompt": "do it",
		"schedule": "0 9 * * 1-5",
		"session_id": "session-456"
	}`))
	if err != nil {
		t.Fatalf("cron_create should succeed even if next_run_at backfill fails: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if id, ok := res["id"].(string); !ok || id == "" {
		t.Fatal("missing id in response despite backfill failure")
	}

	after := metrics.GlobalCronNextRunWriteFailuresTotal.Load()
	if after != before+1 {
		t.Errorf("expected GlobalCronNextRunWriteFailuresTotal += 1, got before=%d after=%d", before, after)
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

	fn := MakeCronListFn(repo.NewSQLiteCronRepository(db))

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

	fn := MakeCronDeleteFn(repo.NewSQLiteCronRepository(db))

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
