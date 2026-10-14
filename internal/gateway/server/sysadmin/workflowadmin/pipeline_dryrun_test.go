package workflowadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	repoProto "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type dryRunMockToolExecutor struct {
	lastArgs map[string][]byte
}

func (m *dryRunMockToolExecutor) Lookup(name string) (types.Tool, error) {
	if name == "feed_fetch" {
		return types.Tool{
			Name:       name,
			Capability: types.CapReadOnly,
			Source:     types.ToolBuiltin,
		}, nil
	}
	if name == "knowledge_ingest" {
		return types.Tool{
			Name:        name,
			Capability:  types.CapWriteLocal,
			SideEffects: []types.SideEffect{types.SideStateMutate},
			Source:      types.ToolBuiltin,
		}, nil
	}
	return types.Tool{}, nil
}

func (m *dryRunMockToolExecutor) ExecuteWithTaint(ctx context.Context, name string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	if m.lastArgs == nil {
		m.lastArgs = make(map[string][]byte)
	}
	m.lastArgs[name] = args

	if name == "feed_fetch" {
		// 返回包含 10 条条目的模拟数据
		items := make([]map[string]any, 10)
		for i := 0; i < 10; i++ {
			items[i] = map[string]any{
				"title":   "AI News Item Title",
				"url":     "https://example.com/news",
				"excerpt": "This is a detailed excerpt about AI breakthroughs.",
			}
		}
		raw, _ := json.Marshal(map[string]any{"items": items})
		return &types.ToolResult{Success: true, Output: raw}, nil
	}
	return &types.ToolResult{Success: true, Output: []byte(`{}`)}, nil
}

func setupDryRunTestDB(t *testing.T) (*sql.DB, repoProto.WorkflowRepository) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite3: %v", err)
	}
	db.SetMaxOpenConns(1)

	schema := `
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
	return db, repo.NewSQLiteWorkflowRepository(db)
}

func TestWorkflowAdmin_DryRunPipeline(t *testing.T) {
	db, r := setupDryRunTestDB(t)
	defer db.Close()

	toolExec := &dryRunMockToolExecutor{}
	h := &WorkflowAdmin{
		DB:           db,
		WorkflowRepo: r,
		ToolExecutor: toolExec,
	}

	spec := pipeline.Spec{
		Kind: "pipeline",
		Name: "AI Daily Digest",
		Trigger: pipeline.Trigger{
			Type: "cron",
			Cron: "0 9 * * *",
		},
		Steps: []pipeline.StepSpec{
			{
				Key:  "fetch",
				Kind: "tool",
				Tool: "feed_fetch",
				Args: json.RawMessage(`{"url":"https://example.com/rss","limit":20}`),
			},
			{
				Key:   "dedup_limit",
				Kind:  "transform",
				Needs: []string{"fetch"},
				Ops: []pipeline.Op{
					{Op: "limit", N: 10},
				},
			},
			{
				Key:    "agent_step",
				Kind:   "agent",
				Needs:  []string{"dedup_limit"},
				Prompt: "Review and format the items",
			},
			{
				Key:             "summarize",
				Kind:            "llm",
				Needs:           []string{"dedup_limit"},
				Input:           "dedup_limit",
				Prompt:          "Summarize top 3 items",
				MaxOutputTokens: 500,
			},
			{
				Key:   "save_knowledge",
				Kind:  "tool",
				Tool:  "knowledge_ingest",
				Needs: []string{"dedup_limit"},
				Args:  json.RawMessage(`{"collection":"ai_news","docs":[{"content":"abc"}]}`),
			},
		},
	}

	ctx := context.Background()
	res, fieldErrs, err := h.DryRunPipeline(ctx, spec)
	if err != nil {
		t.Fatalf("DryRunPipeline unexpected error: %v", err)
	}
	if len(fieldErrs) > 0 {
		t.Fatalf("DryRunPipeline unexpected validation errors: %+v", fieldErrs)
	}

	if len(res.Steps) != 5 {
		t.Fatalf("expected 5 steps in result, got %d", len(res.Steps))
	}

	// 1. 验证 tool 步骤参数 limit 被夹紧为 5
	rawArgs := toolExec.lastArgs["feed_fetch"]
	var parsedArgs map[string]any
	_ = json.Unmarshal(rawArgs, &parsedArgs)
	if limitVal, ok := parsedArgs["limit"].(float64); !ok || limitVal != 5 {
		t.Errorf("expected limit clamped to 5, got %v", parsedArgs["limit"])
	}

	// 2. 验证 preview 每步最多 3 条，每条 <= 120 rune
	for _, step := range res.Steps {
		if len(step.Preview) > 3 {
			t.Errorf("step %s preview items %d > 3", step.Key, len(step.Preview))
		}
		for _, p := range step.Preview {
			if len([]rune(p)) > 120 {
				t.Errorf("step %s preview rune length %d > 120", step.Key, len([]rune(p)))
			}
		}
	}

	// 3. 验证 agent 步骤状态为 not_run
	foundAgent := false
	for _, step := range res.Steps {
		if step.Key == "agent_step" {
			foundAgent = true
			if step.Status != "not_run" {
				t.Errorf("expected agent_step status 'not_run', got %q", step.Status)
			}
			if step.Note != "agent 步骤不在试运行中执行" {
				t.Errorf("expected note 'agent 步骤不在试运行中执行', got %q", step.Note)
			}
		}
	}
	if !foundAgent {
		t.Error("agent_step not found in dry run results")
	}

	// 4. 验证写入类工具 knowledge_ingest 在 dry-run 中跳过真调用
	if _, called := toolExec.lastArgs["knowledge_ingest"]; called {
		t.Error("knowledge_ingest should NOT be called in dry-run mode")
	}

	// 5. 验证 Token 估算大于 0
	if res.EstimatedLLMInputTokens <= 0 {
		t.Errorf("expected EstimatedLLMInputTokens > 0, got %d", res.EstimatedLLMInputTokens)
	}
	if res.EstimatedLLMOutputTokensMax <= 0 {
		t.Errorf("expected EstimatedLLMOutputTokensMax > 0, got %d", res.EstimatedLLMOutputTokensMax)
	}

	// 6. 验证整体序列化结果 <= 2000 rune
	b, _ := json.Marshal(res)
	if len([]rune(string(b))) > 2000 {
		t.Errorf("expected result serialized <= 2000 runes, got %d", len([]rune(string(b))))
	}
}

func TestWorkflowAdmin_DryRunWorkflow(t *testing.T) {
	db, r := setupDryRunTestDB(t)
	defer db.Close()

	toolExec := &dryRunMockToolExecutor{}
	h := &WorkflowAdmin{
		DB:           db,
		WorkflowRepo: r,
		ToolExecutor: toolExec,
	}

	spec := pipeline.Spec{
		Kind: "pipeline",
		Name: "Existing Workflow",
		Trigger: pipeline.Trigger{
			Type: "cron",
			Cron: "0 10 * * *",
		},
		Steps: []pipeline.StepSpec{
			{
				Key:  "fetch",
				Kind: "tool",
				Tool: "feed_fetch",
				Args: json.RawMessage(`{"url":"https://example.com/rss"}`),
			},
		},
	}

	ctx := context.Background()
	wfID, _, errs, err := h.SavePipeline(ctx, spec, "", "agent")
	if err != nil || len(errs) > 0 {
		t.Fatalf("SavePipeline failed: err=%v, errs=%+v", err, errs)
	}

	dryRes, err := h.DryRunWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("DryRunWorkflow failed: %v", err)
	}
	if len(dryRes.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dryRes.Steps))
	}
	if dryRes.Steps[0].Key != "fetch" || dryRes.Steps[0].Status != "ok" {
		t.Errorf("unexpected step result: %+v", dryRes.Steps[0])
	}
}
