package workflowadmin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestWorkflowHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS workflows (
			id TEXT PRIMARY KEY,
			type TEXT DEFAULT 'chain',
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
			updated_at DATETIME,
			result_action TEXT DEFAULT 'silent',
			deliver_from TEXT DEFAULT '',
			source TEXT DEFAULT 'user'
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
			step_key TEXT DEFAULT '',
			kind TEXT DEFAULT 'agent',
			config_json TEXT DEFAULT '{}',
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
			step_outputs    TEXT    NOT NULL DEFAULT '[]',
			skipped         INTEGER DEFAULT 0
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO workflows (id, name, description, trigger_type, cron_schedule, enabled, status, last_run_status, created_at, updated_at) VALUES ('1', 'test', '', '', '', 1, 'active', 'ok', '2024', '2024')`)
	if err != nil {
		t.Fatal(err)
	}

	h := &WorkflowAdmin{
		DB:           db,
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

func TestWorkflowHandlers_Pipeline(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS workflows (
			id TEXT PRIMARY KEY,
			type TEXT DEFAULT 'chain',
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
			updated_at DATETIME,
			result_action TEXT DEFAULT 'silent',
			deliver_from TEXT DEFAULT '',
			source TEXT DEFAULT 'user'
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
			step_key TEXT DEFAULT '',
			kind TEXT DEFAULT 'agent',
			config_json TEXT DEFAULT '{}',
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
			step_outputs    TEXT    NOT NULL DEFAULT '[]',
			skipped         INTEGER DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS workflow_run_data (
			run_id       TEXT    NOT NULL,
			step_key     TEXT    NOT NULL,
			status       TEXT    NOT NULL,
			data_json    BLOB    NOT NULL,
			item_count   INTEGER NOT NULL DEFAULT 0,
			created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
			PRIMARY KEY (run_id, step_key)
		);
		CREATE TABLE IF NOT EXISTS llm_calls (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			purpose TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	toolExec := &mockToolExec{}
	wfRepo := repo.NewSQLiteWorkflowRepository(db)
	h := &WorkflowAdmin{
		DB:           db,
		WorkflowRepo: wfRepo,
		ToolExecutor: toolExec,
	}

	// 1. 创建合法 pipeline 格式工作流
	pipelineBody := `{
		"kind": "pipeline",
		"name": "Daily Tech Digest",
		"description": "Tech news aggregator",
		"trigger": { "type": "cron", "cron": "0 8 * * *" },
		"result_action": "channel:tech",
		"deliver_from": "digest",
		"steps": [
			{
				"key": "feed",
				"kind": "tool",
				"tool": "feed_fetch",
				"args": { "url": "https://example.com/rss" }
			},
			{
				"key": "digest",
				"kind": "llm",
				"needs": ["feed"],
				"input": "feed",
				"prompt": "总结新闻"
			}
		]
	}`
	req := httptest.NewRequest("POST", "/v1/workflows", bytes.NewBufferString(pipelineBody))
	w := httptest.NewRecorder()
	h.HandleCreateWorkflow(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create pipeline workflow failed (%d): %v", w.Result().StatusCode, w.Body.String())
	}
	var createResp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&createResp)
	createdID := createResp["id"]
	if createdID == "" {
		t.Fatal("expected non-empty created workflow id")
	}

	// 验证落库属性
	var wfType, resAct, delivFrom, src string
	if err := db.QueryRow(`SELECT type, result_action, deliver_from, source FROM workflows WHERE id=?`, createdID).Scan(&wfType, &resAct, &delivFrom, &src); err != nil {
		t.Fatalf("query workflow: %v", err)
	}
	if wfType != "dag" || resAct != "channel:tech" || delivFrom != "digest" || src != "user" {
		t.Errorf("unexpected workflow row: type=%s, result_action=%s, deliver_from=%s, source=%s", wfType, resAct, delivFrom, src)
	}

	// 2. 创建非法 pipeline 格式工作流（循环依赖） -> 400 with errors
	invalidBody := `{
		"kind": "pipeline",
		"name": "Bad Pipeline",
		"trigger": { "type": "manual" },
		"steps": [
			{ "key": "a", "kind": "transform", "needs": ["b"], "ops": [{"op": "limit", "n": 5}] },
			{ "key": "b", "kind": "transform", "needs": ["a"], "ops": [{"op": "limit", "n": 5}] }
		]
	}`
	req = httptest.NewRequest("POST", "/v1/workflows", bytes.NewBufferString(invalidBody))
	w = httptest.NewRecorder()
	h.HandleCreateWorkflow(w, req)
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for cyclic pipeline, got %d", w.Result().StatusCode)
	}
	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if _, ok := errResp["errors"]; !ok {
		t.Errorf("expected errors in response: %v", errResp)
	}

	// 3. SavePipeline 更新
	spec := pipeline.Spec{
		Kind:         "pipeline",
		Name:         "Daily Tech Digest Updated",
		Trigger:      pipeline.Trigger{Type: "cron", Cron: "0 9 * * *"},
		ResultAction: "session",
		DeliverFrom:  "digest",
		Steps: []pipeline.StepSpec{
			{Key: "feed", Kind: "tool", Tool: "feed_fetch", Args: []byte(`{"url":"https://example.com/rss"}`)},
			{Key: "digest", Kind: "llm", Needs: []string{"feed"}, Input: "feed", Prompt: "总结新闻更新"},
		},
	}
	updatedID, _, errs, err := h.SavePipeline(context.Background(), spec, createdID, "user")
	if len(errs) > 0 || err != nil {
		t.Fatalf("SavePipeline update failed: errs=%v, err=%v", errs, err)
	}
	if updatedID != createdID {
		t.Errorf("expected same id %s, got %s", createdID, updatedID)
	}

	// 4. HandleGetRunStepData 与截断
	items := make([]map[string]any, 60)
	for i := 0; i < 60; i++ {
		items[i] = map[string]any{"title": fmt.Sprintf("Item %d", i)}
	}
	itemsJSON, _ := json.Marshal(pipeline.StepData{Items: items, Count: 60})
	_ = wfRepo.SaveRunData(context.Background(), "r-100", "fetch", "ok", itemsJSON, 60)

	req = httptest.NewRequest("GET", "/v1/workflows/runs/r-100/data/fetch", nil)
	req.SetPathValue("id", "r-100")
	req.SetPathValue("step_key", "fetch")
	w = httptest.NewRecorder()
	h.HandleGetRunStepData(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("HandleGetRunStepData failed (%d): %v", w.Result().StatusCode, w.Body.String())
	}
	var dataResp struct {
		Status    string           `json:"status"`
		Count     int              `json:"count"`
		Truncated bool             `json:"truncated"`
		Items     []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(w.Body).Decode(&dataResp)
	if dataResp.Count != 60 || !dataResp.Truncated || len(dataResp.Items) != 50 {
		t.Errorf("unexpected step data resp: count=%d, truncated=%v, len(items)=%d", dataResp.Count, dataResp.Truncated, len(dataResp.Items))
	}

	// 不存在步骤数据返回 404
	req = httptest.NewRequest("GET", "/v1/workflows/runs/r-100/data/nonexistent", nil)
	req.SetPathValue("id", "r-100")
	req.SetPathValue("step_key", "nonexistent")
	w = httptest.NewRecorder()
	h.HandleGetRunStepData(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent step data, got %d", w.Result().StatusCode)
	}

	// 5. HandleListWorkflowRuns 统计验证
	_, _ = db.Exec(`INSERT INTO workflow_runs (id, workflow_id, trigger, status, step_outputs, skipped)
		VALUES ('r-100', ?, 'cron', 'ok', '[{"seq":0,"session_id":"s-agent-1"}]', 1)`, createdID)
	_, _ = db.Exec(`INSERT INTO llm_calls (id, session_id, purpose, input_tokens, output_tokens, cost_usd)
		VALUES ('call-1', 'r-100', 'workflow_llm_step', 100, 50, 0.005),
		       ('call-2', 's-agent-1', 'execute', 200, 100, 0.010)`)

	req = httptest.NewRequest("GET", "/v1/workflows/"+createdID+"/runs", nil)
	req.SetPathValue("id", createdID)
	w = httptest.NewRecorder()
	h.HandleListWorkflowRuns(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("HandleListWorkflowRuns failed (%d): %v", w.Result().StatusCode, w.Body.String())
	}
	var runsResp struct {
		Runs []workflowRun `json:"runs"`
	}
	_ = json.NewDecoder(w.Body).Decode(&runsResp)
	if len(runsResp.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runsResp.Runs))
	}
	r0 := runsResp.Runs[0]
	if r0.Skipped != 1 {
		t.Errorf("expected skipped=1, got %d", r0.Skipped)
	}
	if r0.LLMTokensIn != 300 || r0.LLMTokensOut != 150 {
		t.Errorf("expected tokens in=300, out=150; got in=%d, out=%d", r0.LLMTokensIn, r0.LLMTokensOut)
	}
	if r0.CostUSD < 0.014 || r0.CostUSD > 0.016 {
		t.Errorf("expected cost_usd around 0.015, got %f", r0.CostUSD)
	}
}
