package sysadmin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestWorkflowHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS workflows (
			id TEXT PRIMARY KEY,
			name TEXT,
			description TEXT,
			trigger_type TEXT,
			cron_schedule TEXT,
			enabled INTEGER,
			status TEXT,
			last_run_status TEXT,
			last_run_error TEXT,
			last_run_at DATETIME,
			next_run_at DATETIME,
			run_count INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS workflow_steps (
			id TEXT PRIMARY KEY,
			workflow_id TEXT,
			seq INTEGER,
			name TEXT,
			automation_id TEXT,
			prompt TEXT,
			reasoning_effort TEXT,
			working_dir TEXT,
			input_from_prev INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS workflow_runs (
			id              TEXT    PRIMARY KEY,
			workflow_id     TEXT    NOT NULL,
			trigger         TEXT    NOT NULL DEFAULT 'manual',
			status          TEXT    NOT NULL DEFAULT 'running',
			current_step    INTEGER NOT NULL DEFAULT 0,
			total_steps     INTEGER NOT NULL DEFAULT 0,
			started_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
			finished_at     TEXT    NOT NULL DEFAULT '',
			error_msg       TEXT    NOT NULL DEFAULT '',
			step_outputs    TEXT    NOT NULL DEFAULT '[]'
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO workflows (id, name, description, trigger_type, cron_schedule, enabled, status, last_run_status, created_at, updated_at) VALUES ('1', 'test', '', '', '', 1, 'active', 'ok', '2024', '2024')`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		WorkflowRepo: repo.NewSQLiteWorkflowRepository(db),
	}

	// List
	req := httptest.NewRequest("GET", "/api/v1/workflows", nil)
	w := httptest.NewRecorder()
	h.HandleListWorkflows(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list workflows failed: %v", w.Body.String())
	}

	// Create
	body := `{"name": "test-wf", "description": "desc", "steps": [{"name": "echo", "prompt": "hello"}]}`
	req = httptest.NewRequest("POST", "/api/v1/workflows", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreateWorkflow(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create workflow failed: %v", w.Body.String())
	}

	// Update
	body = `{"name": "test-wf2", "enabled": true}`
	req = httptest.NewRequest("PUT", "/api/v1/workflows/1", bytes.NewBufferString(body))
	req.SetPathValue("id", "1")
	w = httptest.NewRecorder()
	h.HandleUpdateWorkflow(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("update workflow failed: %v", w.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/v1/workflows/1", nil)
	req.SetPathValue("id", "1")
	w = httptest.NewRecorder()
	h.HandleDeleteWorkflow(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("delete workflow failed: %v", w.Body.String())
	}
}
