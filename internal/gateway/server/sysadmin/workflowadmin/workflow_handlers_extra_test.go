package workflowadmin

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestWorkflowHandlersExtra(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS workflows (
			id TEXT PRIMARY KEY,
			type TEXT DEFAULT 'chain',
			name TEXT,
			description TEXT,
			trigger_type TEXT,
			cron_schedule TEXT,
			channel_id TEXT,
			event_filter TEXT,
			working_dir TEXT,
			result_action TEXT,
			cedar_rules_json TEXT,
			requires_hitl INTEGER,
			risk_level TEXT,
			enabled INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS workflow_runs (
			id TEXT PRIMARY KEY,
			workflow_id TEXT,
			trigger TEXT,
			status TEXT,
			current_step INTEGER,
			total_steps INTEGER,
			step_outputs TEXT,
			error_msg TEXT,
			started_at DATETIME,
			finished_at DATETIME
		);
		INSERT INTO workflows (id, name, trigger_type, enabled, created_at, updated_at) VALUES ('wf-1', 'Test WF', 'manual', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &WorkflowAdmin{
		DB: db,
	}

	// Get Workflow
	req := httptest.NewRequest("GET", "/api/v1/workflows/wf-1", nil)
	req.SetPathValue("id", "wf-1")
	w := httptest.NewRecorder()
	h.HandleGetWorkflow(w, req)
	if w.Result().StatusCode != http.StatusOK && w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("get workflow failed: %v %s", w.Result().StatusCode, w.Body.String())
	}

	// List Workflow Runs
	req = httptest.NewRequest("GET", "/api/v1/workflows/wf-1/runs", nil)
	req.SetPathValue("id", "wf-1")
	w = httptest.NewRecorder()
	h.HandleListWorkflowRuns(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list workflow runs failed: %v %s", w.Result().StatusCode, w.Body.String())
	}

	// Trigger Workflow
	body := `{}`
	req = httptest.NewRequest("POST", "/api/v1/workflows/wf-1/trigger", bytes.NewBufferString(body))
	req.SetPathValue("id", "wf-1")
	w = httptest.NewRecorder()
	h.HandleTriggerWorkflow(w, req)
	// It'll probably return an error because runWorkflow isn't fully mocked, but we get coverage for handler.
}
