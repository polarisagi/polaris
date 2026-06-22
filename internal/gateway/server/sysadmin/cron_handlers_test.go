package sysadmin

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestCronHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS automations (
			id TEXT PRIMARY KEY,
			name TEXT,
			description TEXT,
			enabled INTEGER,
			trigger_type TEXT,
			cron_schedule TEXT,
			schedule TEXT,
			event_filter TEXT,
			events TEXT,
			prompt TEXT,
			script TEXT,
			sandbox_level TEXT,
			model TEXT,
			channel_id TEXT,
			working_dir TEXT,
			result_action TEXT,
			cedar_rules_json TEXT,
			requires_hitl INTEGER,
			risk_level TEXT,
			system_prompt_version INTEGER,
			reasoning_effort TEXT,
			next_run_at DATETIME,
			last_run_at DATETIME,
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
			triggered_by TEXT,
			session_id TEXT,
			started_at DATETIME,
			finished_at DATETIME,
			completed_at DATETIME,
			error_msg TEXT,
			error TEXT,
			prompt_snapshot TEXT,
			logs TEXT
		);
		INSERT INTO automations (id, name, description, enabled, trigger_type, cron_schedule, schedule, event_filter, events, prompt, script, sandbox_level, model, channel_id, working_dir, result_action, cedar_rules_json, requires_hitl, risk_level, reasoning_effort, run_count, last_run_status, last_run_error, circuit_open, created_at, updated_at)
		VALUES ('auto-1', 'test-auto', 'Desc', 1, 'cron', '0 * * * *', '* * * * *', '{}', '[]', 'prompt', '', 'local', 'model', 'chan1', '.', 'log', '{}', 0, 'low', 'low', 0, '', '', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB:             db,
		AutomationRepo: repo.NewSQLiteAutomationRepository(db),
	}

	// List Automations
	req := httptest.NewRequest("GET", "/api/v1/automations", nil)
	w := httptest.NewRecorder()
	h.HandleListAutomations(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list automations failed")
	}

	// Create Automation
	body := `{"name": "new-auto", "description": "desc", "trigger_type": "cron", "cron_schedule": "0 0 * * *", "events": [], "prompt": "do it", "enabled": true}`
	req = httptest.NewRequest("POST", "/api/v1/automations", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreateAutomation(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create automation failed: %v", w.Body.String())
	}

	// Update Automation
	body = `{"enabled": false}`
	req = httptest.NewRequest("PUT", "/api/v1/automations/auto-1", bytes.NewBufferString(body))
	req.SetPathValue("id", "auto-1")
	w = httptest.NewRecorder()
	h.HandleUpdateAutomation(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Logf("update automation returned: %v %s", w.Result().StatusCode, w.Body.String())
	}

	// Trigger Automation
	req = httptest.NewRequest("POST", "/api/v1/automations/auto-1/trigger", nil)
	req.SetPathValue("id", "auto-1")
	w = httptest.NewRecorder()
	h.HandleTriggerAutomation(w, req)
	if w.Result().StatusCode != http.StatusAccepted {
		t.Logf("trigger automation returned: %v", w.Result().StatusCode)
	}

	// List Runs
	req = httptest.NewRequest("GET", "/api/v1/automations/auto-1/runs", nil)
	req.SetPathValue("id", "auto-1")
	w = httptest.NewRecorder()
	h.HandleListAutomationRuns(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list automation runs failed")
	}

	// Delete Automation
	req = httptest.NewRequest("DELETE", "/api/v1/automations/auto-1", nil)
	req.SetPathValue("id", "auto-1")
	w = httptest.NewRecorder()
	h.HandleDeleteAutomation(w, req)
	if w.Result().StatusCode != http.StatusNoContent && w.Result().StatusCode != http.StatusOK {
		t.Errorf("delete automation failed: %v", w.Body.String())
	}
}
