package cronadmin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestCronRunnerExtra(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS automations (
			id TEXT PRIMARY KEY,
			name TEXT,
			prompt TEXT,
			trigger_type TEXT,
			cron_schedule TEXT,
			event_filter TEXT,
			working_dir TEXT,
			env_type TEXT,
			reasoning_effort TEXT,
			result_action TEXT,
			sandbox_level INTEGER,
			cedar_rules_json TEXT,
			channel_id TEXT,
			enabled INTEGER,
			requires_hitl INTEGER,
			risk_level INTEGER,
			last_run_at TEXT,
			next_run_at TEXT,
			run_count INTEGER,
			last_run_status TEXT,
			last_run_error TEXT,
			circuit_open INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS automation_runs (
			id TEXT PRIMARY KEY,
			automation_id TEXT,
			trigger TEXT,
			status TEXT,
			session_id TEXT,
			started_at DATETIME,
			finished_at DATETIME,
			error_msg TEXT,
			prompt_snapshot TEXT
		);
		CREATE TABLE IF NOT EXISTS events (
			offset INTEGER PRIMARY KEY,
			topic TEXT,
			type TEXT,
			payload TEXT
		);
		INSERT INTO automations (id, name, trigger_type, cron_schedule, enabled, last_run_status) VALUES ('a-cron', 'Cron', 'cron', '* * * * *', 1, '');
		INSERT INTO automations (id, name, trigger_type, event_filter, enabled, last_run_status) VALUES ('a-event', 'Event', 'event', '{"type":"webhook"}', 1, '');
	`)
	if err != nil {
		t.Fatal(err)
	}

	ca := &CronAdmin{
		DB:             db,
		AutomationRepo: repo.NewSQLiteAutomationRepository(db),
		EventRepo:      repo.NewSQLiteEventRepository(db),
		// GD-9-001 复核修复后 cronTick 真正查到到期任务并跑到这里；此前查询因
		// schema 缺列提前报错返回，从未真正调用到 CronTickWorkflows，掩盖了这里
		// 缺桩的问题。补一个 no-op，与 cronTick 末尾的调用对齐。
		CronTickWorkflows: func(ctx context.Context) {},
	}

	// Just call cronTick and eventTick, we don't need them to do everything, just run the queries
	ca.cronTick(context.Background())
	ca.eventTick(context.Background())

	// Also list automation templates
	req := httptest.NewRequest("GET", "/api/v1/automations/templates", nil)
	w := httptest.NewRecorder()
	ca.HandleListAutomationTemplates(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list automation templates failed: %v", w.Result().StatusCode)
	}

	// Coverage for runner
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	ca.StartCronRunner(ctx)
}
