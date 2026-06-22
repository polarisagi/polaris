package sysadmin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
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
			trigger_type TEXT,
			cron_schedule TEXT,
			event_filter TEXT,
			script TEXT,
			result_action TEXT,
			channel_id TEXT,
			enabled INTEGER,
			run_count INTEGER,
			circuit_open INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS automation_runs (
			id TEXT PRIMARY KEY,
			automation_id TEXT,
			status TEXT,
			started_at DATETIME,
			completed_at DATETIME
		);
		INSERT INTO automations (id, name, trigger_type, cron_schedule, enabled) VALUES ('a-cron', 'Cron', 'cron', '* * * * *', 1);
		INSERT INTO automations (id, name, trigger_type, event_filter, enabled) VALUES ('a-event', 'Event', 'event', '{"type":"webhook"}', 1);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB: db,
	}

	// Just call cronTick and eventTick, we don't need them to do everything, just run the queries
	h.cronTick(context.Background())
	h.eventTick(context.Background())

	// Also list automation templates
	req := httptest.NewRequest("GET", "/api/v1/automations/templates", nil)
	w := httptest.NewRecorder()
	h.HandleListAutomationTemplates(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list automation templates failed: %v", w.Result().StatusCode)
	}

	// Coverage for runner
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	h.startCronRunner(ctx)
}
