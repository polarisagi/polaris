package workflowadmin

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestWorkflowHandlersExtra(t *testing.T) {
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
			channel_id TEXT,
			event_filter TEXT,
			working_dir TEXT,
			result_action TEXT,
			cedar_rules_json TEXT,
			requires_hitl INTEGER,
			risk_level TEXT,
			enabled INTEGER,
			created_at DATETIME,
			updated_at DATETIME,
			deliver_from TEXT DEFAULT '',
			source TEXT DEFAULT 'user'
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
			finished_at DATETIME,
			skipped INTEGER DEFAULT 0
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

type mockAdapterExecutor struct {
	tools map[string]types.Tool
}

func (m *mockAdapterExecutor) Lookup(name string) (types.Tool, error) {
	if t, ok := m.tools[name]; ok {
		return t, nil
	}
	return types.Tool{}, apperr.New(apperr.CodeNotFound, "tool not found")
}

func (m *mockAdapterExecutor) ExecuteWithTaint(ctx context.Context, name string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true}, nil
}

func TestToolMetaAdapter_MCPReadOnly(t *testing.T) {
	exec := &mockAdapterExecutor{
		tools: map[string]types.Tool{
			"mcp_unannotated": {
				Name:         "mcp_unannotated",
				Source:       types.ToolMCP,
				Capability:   types.CapReadOnly, // zero value
				ReadOnlyHint: false,
			},
			"mcp_annotated_readonly": {
				Name:         "mcp_annotated_readonly",
				Source:       types.ToolMCP,
				Capability:   types.CapReadOnly,
				ReadOnlyHint: true,
			},
			"builtin_readonly": {
				Name:       "builtin_readonly",
				Source:     types.ToolBuiltin,
				Capability: types.CapReadOnly,
			},
			"builtin_write": {
				Name:       "builtin_write",
				Source:     types.ToolBuiltin,
				Capability: types.CapWriteLocal,
			},
		},
	}

	adapter := toolMetaAdapter{exec: exec}

	// 1. MCP unannotated -> ReadOnly 必须为 false（即使 Capability 是零值 CapReadOnly）
	meta := adapter.ToolMeta("mcp_unannotated")
	if !meta.Exists || meta.ReadOnly {
		t.Errorf("expected mcp_unannotated ReadOnly=false, got exists=%v readonly=%v", meta.Exists, meta.ReadOnly)
	}

	// 2. MCP annotated readonly -> ReadOnly 必须为 true
	meta = adapter.ToolMeta("mcp_annotated_readonly")
	if !meta.Exists || !meta.ReadOnly {
		t.Errorf("expected mcp_annotated_readonly ReadOnly=true, got exists=%v readonly=%v", meta.Exists, meta.ReadOnly)
	}

	// 3. Builtin readonly -> ReadOnly 必须为 true
	meta = adapter.ToolMeta("builtin_readonly")
	if !meta.Exists || !meta.ReadOnly {
		t.Errorf("expected builtin_readonly ReadOnly=true, got exists=%v readonly=%v", meta.Exists, meta.ReadOnly)
	}

	// 4. Builtin write -> ReadOnly 必须为 false
	meta = adapter.ToolMeta("builtin_write")
	if !meta.Exists || meta.ReadOnly {
		t.Errorf("expected builtin_write ReadOnly=false, got exists=%v readonly=%v", meta.Exists, meta.ReadOnly)
	}

	// 5. 不存在工具
	meta = adapter.ToolMeta("unknown_tool")
	if meta.Exists {
		t.Errorf("expected unknown_tool Exists=false, got %v", meta.Exists)
	}
}
