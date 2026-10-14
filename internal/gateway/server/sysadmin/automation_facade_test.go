package sysadmin

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/workflowadmin"
	"github.com/polarisagi/polaris/internal/protocol"
	repoimpl "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type facadeMockExecutor struct{}

func (m *facadeMockExecutor) Lookup(name string) (types.Tool, error) {
	if name == "feed_fetch" {
		return types.Tool{
			Name:       name,
			Capability: types.CapReadOnly,
			Source:     types.ToolBuiltin,
		}, nil
	}
	return types.Tool{}, nil
}

func (m *facadeMockExecutor) ExecuteWithTaint(ctx context.Context, name string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{
		Success: true,
		Output:  []byte(`{"items":[{"title":"Item 1","url":"https://example.com"}]}`),
	}, nil
}

func setupFacadeTest(t *testing.T) (*AutomationFacade, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite3: %v", err)
	}
	db.SetMaxOpenConns(1)

	schema := `
	CREATE TABLE automations (
		id TEXT PRIMARY KEY,
		name TEXT,
		prompt TEXT,
		trigger_type TEXT,
		cron_schedule TEXT,
		event_filter TEXT NOT NULL DEFAULT '{}',
		channel_id TEXT,
		working_dir TEXT,
		env_type TEXT,
		reasoning_effort TEXT,
		result_action TEXT,
		sandbox_level INTEGER,
		cedar_rules_json TEXT,
		enabled INTEGER,
		requires_hitl INTEGER,
		risk_level INTEGER,
		source TEXT DEFAULT 'user',
		last_run_at TEXT,
		next_run_at TEXT,
		run_count INTEGER,
		last_run_status TEXT,
		last_run_error TEXT,
		consecutive_errors INTEGER DEFAULT 0,
		circuit_open INTEGER DEFAULT 0,
		circuit_opened_at TEXT,
		created_at DATETIME,
		updated_at DATETIME
	);
	CREATE TABLE cron_jobs (
		id TEXT PRIMARY KEY,
		name TEXT,
		cron_expr TEXT,
		next_run_at DATETIME,
		last_run_at DATETIME,
		last_status TEXT,
		last_error TEXT,
		run_count INTEGER DEFAULT 0,
		enabled INTEGER DEFAULT 1,
		created_at DATETIME,
		updated_at DATETIME,
		source TEXT DEFAULT 'user'
	);
	CREATE TABLE workflows (
		id              TEXT PRIMARY KEY,
		type            TEXT NOT NULL DEFAULT 'chain',
		name            TEXT NOT NULL DEFAULT '',
		description     TEXT NOT NULL DEFAULT '',
		trigger_type    TEXT NOT NULL DEFAULT 'manual',
		cron_schedule   TEXT NOT NULL DEFAULT '',
		enabled         INTEGER NOT NULL DEFAULT 1,
		result_action   TEXT NOT NULL DEFAULT 'session',
		deliver_from    TEXT NOT NULL DEFAULT '',
		source          TEXT NOT NULL DEFAULT 'user',
		last_run_at     TEXT NOT NULL DEFAULT '',
		next_run_at     TEXT NOT NULL DEFAULT '',
		run_count       INTEGER NOT NULL DEFAULT 0,
		last_run_status TEXT NOT NULL DEFAULT '',
		last_run_error  TEXT NOT NULL DEFAULT '',
		failure_count   INTEGER NOT NULL DEFAULT 0,
		circuit_open    INTEGER NOT NULL DEFAULT 0,
		circuit_opened_at TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL DEFAULT '',
		updated_at      TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE workflow_steps (
		id               TEXT PRIMARY KEY,
		workflow_id      TEXT NOT NULL,
		seq              INTEGER NOT NULL,
		name             TEXT NOT NULL DEFAULT '',
		automation_id    TEXT NOT NULL DEFAULT '',
		prompt           TEXT NOT NULL DEFAULT '',
		reasoning_effort TEXT NOT NULL DEFAULT 'medium',
		working_dir      TEXT NOT NULL DEFAULT '',
		input_from_prev  INTEGER NOT NULL DEFAULT 1,
		depends_on       TEXT NOT NULL DEFAULT '[]',
		capability_type  TEXT NOT NULL DEFAULT '',
		compensation_tool TEXT NOT NULL DEFAULT '',
		compensation_args TEXT NOT NULL DEFAULT '',
		max_retries      INTEGER NOT NULL DEFAULT 0,
		step_key         TEXT NOT NULL DEFAULT '',
		kind             TEXT NOT NULL DEFAULT 'agent',
		config_json      TEXT NOT NULL DEFAULT '{}',
		UNIQUE(workflow_id, seq)
	);
	CREATE TABLE workflow_runs (
		id TEXT PRIMARY KEY,
		workflow_id TEXT,
		trigger TEXT,
		status TEXT,
		current_step INTEGER,
		total_steps INTEGER,
		started_at DATETIME,
		finished_at DATETIME,
		error_msg TEXT,
		step_outputs TEXT,
		skipped INTEGER DEFAULT 0,
		result_meta TEXT DEFAULT ''
	);
	CREATE TABLE workflow_seen_items (
		workflow_id TEXT,
		item_key TEXT,
		first_seen_at DATETIME,
		PRIMARY KEY (workflow_id, item_key)
	);
	CREATE TABLE llm_calls (
		id TEXT PRIMARY KEY,
		session_id TEXT,
		task_id TEXT,
		model TEXT,
		purpose TEXT,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cost_usd REAL DEFAULT 0,
		created_at DATETIME
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("init db schema: %v", err)
	}

	autoRepo := repoimpl.NewSQLiteAutomationRepository(db)
	wfRepo := repoimpl.NewSQLiteWorkflowRepository(db)

	cronAdmin := &cronadmin.CronAdmin{
		DB:             db,
		AutomationRepo: autoRepo,
	}

	wfAdmin := &workflowadmin.WorkflowAdmin{
		DB:           db,
		WorkflowRepo: wfRepo,
		ToolExecutor: &facadeMockExecutor{},
	}

	facade := &AutomationFacade{
		Cron:     cronAdmin,
		Workflow: wfAdmin,
	}

	return facade, db
}

func TestAutomationFacade_SavePipelineAndDryRun(t *testing.T) {
	facade, db := setupFacadeTest(t)
	defer db.Close()

	ctx := context.Background()

	// 1. 测试合法 pipeline Spec 的 SavePipeline
	validSpecJSON := []byte(`{
		"kind": "pipeline",
		"name": "Tech News Pipeline",
		"trigger": {"type": "cron", "cron": "0 8 * * *"},
		"steps": [
			{
				"key": "fetch",
				"kind": "tool",
				"tool": "feed_fetch",
				"args": {"url": "https://example.com/feed"}
			}
		]
	}`)

	saveRes, err := facade.SavePipeline(ctx, validSpecJSON, "")
	if err != nil {
		t.Fatalf("SavePipeline unexpected error: %v", err)
	}
	if !saveRes.OK {
		t.Fatalf("SavePipeline expected OK=true, got false: %+v", saveRes.Errors)
	}
	if !strings.HasPrefix(saveRes.ID, "wf_") {
		t.Errorf("expected workflow ID prefix 'wf_', got %q", saveRes.ID)
	}
	wfID := saveRes.ID

	// 2. 测试非法 spec (含有未知工具)
	invalidSpecJSON := []byte(`{
		"kind": "pipeline",
		"name": "Invalid Pipeline",
		"steps": [
			{
				"key": "bad_step",
				"kind": "tool",
				"tool": "non_existent_tool"
			}
		]
	}`)

	badSaveRes, err := facade.SavePipeline(ctx, invalidSpecJSON, "")
	if err != nil {
		t.Fatalf("SavePipeline validation error should not return Go err, got %v", err)
	}
	if badSaveRes.OK {
		t.Error("expected bad save to return OK=false")
	}
	if len(badSaveRes.Errors) == 0 {
		t.Error("expected field errors for non existent tool")
	}

	// 3. 测试 DryRunSpec
	dryRes, fieldErrs, err := facade.DryRunSpec(ctx, validSpecJSON)
	if err != nil || len(fieldErrs) > 0 {
		t.Fatalf("DryRunSpec failed: err=%v, fieldErrs=%+v", err, fieldErrs)
	}
	if len(dryRes.Steps) != 1 || dryRes.Steps[0].Status != "ok" {
		t.Errorf("unexpected dry run steps: %+v", dryRes.Steps)
	}

	// 4. 测试 DryRunByID
	dryResByID, err := facade.DryRunByID(ctx, wfID)
	if err != nil {
		t.Fatalf("DryRunByID failed: %v", err)
	}
	if len(dryResByID.Steps) != 1 || dryResByID.Steps[0].Key != "fetch" {
		t.Errorf("unexpected dry run by ID steps: %+v", dryResByID.Steps)
	}
}

func TestAutomationFacade_ListDeleteAndCount(t *testing.T) {
	facade, db := setupFacadeTest(t)
	defer db.Close()

	ctx := context.Background()

	// 1. 创建 1 个 prompt 自动化
	promptIn := protocol.PromptAutomationInput{
		Name:    "Prompt Task 1",
		Prompt:  "Daily summary prompt",
		Cron:    "0 7 * * *",
		Enabled: true,
	}
	promptID, _, err := facade.SavePrompt(ctx, promptIn)
	if err != nil {
		t.Fatalf("SavePrompt failed: %v", err)
	}

	// 2. 创建 1 个 pipeline 自动化
	pipelineJSON := []byte(`{
		"kind": "pipeline",
		"name": "Pipeline Task 1",
		"trigger": {"type": "cron", "cron": "0 9 * * *"},
		"steps": [
			{"key": "fetch", "kind": "tool", "tool": "feed_fetch", "args": {"url": "https://example.com/feed"}}
		]
	}`)
	pipeRes, err := facade.SavePipeline(ctx, pipelineJSON, "")
	if err != nil || !pipeRes.OK {
		t.Fatalf("SavePipeline failed: err=%v, res=%+v", err, pipeRes)
	}
	pipelineID := pipeRes.ID

	// 3. 测试 List（合并两者，且包含 kind 标注）
	summaries, err := facade.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}
	foundPrompt := false
	foundPipeline := false
	for _, s := range summaries {
		if s.ID == promptID && s.Kind == "prompt" {
			foundPrompt = true
		}
		if s.ID == pipelineID && s.Kind == "pipeline" {
			foundPipeline = true
		}
	}
	if !foundPrompt || !foundPipeline {
		t.Errorf("expected both prompt and pipeline summaries, got %+v", summaries)
	}

	// 4. 测试 CountEnabledBySource（两者之和，source 默认均为 agent）
	count, err := facade.CountEnabledBySource(ctx, "agent")
	if err != nil {
		t.Fatalf("CountEnabledBySource failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected count=2 for source 'agent', got %d", count)
	}

	// 5. 测试 Delete 分派（分别删除 wf_ 和 auto_）
	if err := facade.Delete(ctx, pipelineID); err != nil {
		t.Fatalf("Delete pipeline failed: %v", err)
	}
	if err := facade.Delete(ctx, promptID); err != nil {
		t.Fatalf("Delete prompt failed: %v", err)
	}

	summariesAfter, _ := facade.List(ctx)
	if len(summariesAfter) != 0 {
		t.Errorf("expected 0 summaries after deletion, got %d", len(summariesAfter))
	}
}
