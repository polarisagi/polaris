package workflowadmin

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestWorkflowInternal(t *testing.T) {
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
			circuit_open INTEGER,
			last_run_status TEXT,
			last_run_error TEXT,
			last_run_at DATETIME,
			next_run_at DATETIME,
			run_count INTEGER,
			success_count INTEGER,
			failure_count INTEGER,
			circuit_opened_at DATETIME,
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
			depends_on TEXT DEFAULT '[]',
			capability_type TEXT DEFAULT '',
			compensation_tool TEXT DEFAULT '',
			compensation_args TEXT DEFAULT '',
			max_retries INTEGER DEFAULT 0,
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
		INSERT INTO workflows (id, name, trigger_type, enabled, circuit_open) VALUES ('wf-1', 'WF', 'cron', 1, 0);
		INSERT INTO workflow_steps (id, workflow_id, seq, name, automation_id, prompt, reasoning_effort, working_dir, input_from_prev) VALUES ('step-1', 'wf-1', 1, 'Step 1', '', '', '', '', 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &WorkflowAdmin{
		DB:           db,
		AgentPool:    nil, // tests do not execute actual agent workflows
		WorkflowRepo: repo.NewSQLiteWorkflowRepository(db),
	}

	h.CronTickWorkflows(context.Background())
	h.applyAutomationOverride(context.Background(), "wf-1", nil, nil, nil)
	steps := h.loadWorkflowSteps(context.Background(), "wf-1")
	if len(steps) != 1 {
		t.Errorf("expected 1 step, got %d", len(steps))
	}
	h.updateWorkflowStats("wf-1", "completed", "", "now")
	h.updateWorkflowStats("wf-1", "failed", "error", "now")

	// executeWorkflow
	h.executeWorkflow(context.Background(), &workflow{ID: "wf-1", CronSchedule: ""}, "test")
}
