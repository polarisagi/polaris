package workflowadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockToolExec struct {
	mu    sync.Mutex
	calls []string
	fn    func(ctx context.Context, name string, args []byte, taint types.TaintLevel) (*types.ToolResult, error)
}

func (f *mockToolExec) ExecuteWithTaint(ctx context.Context, name string, args []byte, taint types.TaintLevel) (*types.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.fn != nil {
		return f.fn(ctx, name, args, taint)
	}
	return &types.ToolResult{Success: true, Output: []byte(`{"items":[]}`)}, nil
}

func (f *mockToolExec) Lookup(name string) (types.Tool, error) {
	return types.Tool{
		Name:       name,
		Capability: types.CapReadOnly,
	}, nil
}

type mockInferCall struct {
	msgs    []types.Message
	purpose string
	taskID  string
}

type mockInfer struct {
	mu      sync.Mutex
	records []mockInferCall
}

func (f *mockInfer) Complete(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	o := &types.InferOptions{}
	for _, opt := range opts {
		opt(o)
	}
	taskID, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	f.mu.Lock()
	f.records = append(f.records, mockInferCall{
		msgs:    msgs,
		purpose: o.Purpose,
		taskID:  taskID,
	})
	f.mu.Unlock()
	return &types.ProviderResponse{
		Content: "今日技术早报：Go 1.25 与 Rust 2026 正式发布。",
	}, nil
}

type mockDeliverCall struct {
	resultAction string
	sessionID    string
	title        string
	text         string
}

type mockDeliverer struct {
	mu    sync.Mutex
	calls []mockDeliverCall
}

func (f *mockDeliverer) DeliverResult(ctx context.Context, resultAction, sessionID, title, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mockDeliverCall{
		resultAction: resultAction,
		sessionID:    sessionID,
		title:        title,
		text:         text,
	})
	return nil
}

type mockAdmitter struct {
	admit bool
}

func (a *mockAdmitter) AdmitBackground(work string) (func(), bool) {
	if !a.admit {
		return nil, false
	}
	return func() {}, true
}

func newPipelineTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, f := range []string{"001_events.sql", "007_tasks.sql", "035_task_checkpoints.sql", "029_workflows.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

// TestPipelineIntegration_WorkflowLifecycle 端到端测试管线工作流执行全生命周期：
// 1. feed(tool) -> merged(transform: dedup+limit) -> digest(llm) 首次正常执行（LLM 1 次，投递 1 次，seen 记录 2 条）；
// 2. 立即二次执行同数据（merged count=0 跳过 -> digest 级联跳过 -> run skipped=1，LLM 与投递调用保持 1 次）；
// 3. tool 步骤报错时 seen 不写入；
// 4. Admitter 拒绝准入时立即置 run skipped=1 且 error_msg=resource_pressure。
func TestPipelineIntegration_WorkflowLifecycle(t *testing.T) {
	db := newPipelineTestDB(t)
	bb := orchestrator.NewSQLiteBlackboard(db)
	wfRepo := repo.NewSQLiteWorkflowRepository(db)

	toolExec := &mockToolExec{
		fn: func(ctx context.Context, name string, args []byte, taint types.TaintLevel) (*types.ToolResult, error) {
			raw := `{"items": [
				{"title": "Go 1.25 Released", "url": "https://go.dev/blog/125", "summary": "Go 1.25 notes"},
				{"title": "Rust 2026", "url": "https://rust-lang.org/2026", "summary": "Rust news"}
			]}`
			return &types.ToolResult{Success: true, Output: []byte(raw)}, nil
		},
	}
	inferMock := &mockInfer{}
	deliverer := &mockDeliverer{}
	admitter := &mockAdmitter{admit: true}

	h := &WorkflowAdmin{
		DB:           db,
		WorkflowRepo: wfRepo,
		Blackboard:   bb,
		ToolExecutor: toolExec,
		LLMInfer:     inferMock.Complete,
		Deliverer:    deliverer,
		Admitter:     admitter,
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go func() {
		_ = h.RunStepWorkerLoop(workerCtx)
	}()

	// 插入工作流定义：feed(tool) -> merged(transform) -> digest(llm)
	wfID := "wf-pipeline-1"
	_, err := db.Exec(`INSERT INTO workflows (id, type, name, description, trigger_type, cron_schedule, enabled, result_action, deliver_from, source)
		VALUES (?, 'dag', 'Tech Digest', 'Auto news collector', 'manual', '', 1, 'channel:tech', 'digest', 'user')`, wfID)
	if err != nil {
		t.Fatalf("insert workflow: %v", err)
	}

	step1Config, _ := json.Marshal(pipeline.StepSpec{
		Key:  "feed",
		Kind: "tool",
		Tool: "feed_fetch",
		Args: json.RawMessage(`{"url": "https://example.com/rss"}`),
	})
	step2Config, _ := json.Marshal(pipeline.StepSpec{
		Key:         "merged",
		Kind:        "transform",
		Needs:       []string{"feed"},
		SkipIfEmpty: true,
		Ops: []pipeline.Op{
			{Op: "dedup", By: "url", Scope: "workflow"},
			{Op: "limit", N: 10},
		},
	})
	step3Config, _ := json.Marshal(pipeline.StepSpec{
		Key:    "digest",
		Kind:   "llm",
		Needs:  []string{"merged"},
		Input:  "merged",
		Prompt: "请总结下列新闻：",
	})

	_, err = db.Exec(`INSERT INTO workflow_steps (id, workflow_id, seq, name, depends_on, step_key, kind, config_json)
		VALUES
		('ws-1', ?, 0, 'Feed Fetch', '[]', 'feed', 'tool', ?),
		('ws-2', ?, 1, 'Merge & Dedup', '["0"]', 'merged', 'transform', ?),
		('ws-3', ?, 2, 'Digest LLM', '["1"]', 'digest', 'llm', ?)`,
		wfID, string(step1Config), wfID, string(step2Config), wfID, string(step3Config))
	if err != nil {
		t.Fatalf("insert workflow steps: %v", err)
	}

	wf := &workflow{
		ID:           wfID,
		Type:         "dag",
		Name:         "Tech Digest",
		ResultAction: "channel:tech",
		DeliverFrom:  "digest",
	}

	// ─── 运行 1：首次全流程通过 ────────────────────────────────────────────────
	runID1 := h.executeWorkflow(context.Background(), wf, "manual")
	waitRunStatus(t, db, runID1, "ok")

	// 验证 LLM 记录
	inferMock.mu.Lock()
	if len(inferMock.records) != 1 {
		t.Fatalf("expected 1 LLM infer call, got %d", len(inferMock.records))
	}
	r1 := inferMock.records[0]
	inferMock.mu.Unlock()

	if r1.purpose != types.PurposeWorkflowLLMStep {
		t.Errorf("expected purpose=%s, got %s", types.PurposeWorkflowLLMStep, r1.purpose)
	}
	if r1.taskID != runID1 {
		t.Errorf("expected taskID=%s, got %s", runID1, r1.taskID)
	}

	// 验证结果投递
	deliverer.mu.Lock()
	if len(deliverer.calls) != 1 {
		t.Fatalf("expected 1 deliver call, got %d", len(deliverer.calls))
	}
	d1 := deliverer.calls[0]
	deliverer.mu.Unlock()

	if d1.resultAction != "channel:tech" || d1.title != "Tech Digest" {
		t.Errorf("unexpected deliver call: %+v", d1)
	}

	// 验证 seen 写入 2 条记录
	var seenCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_seen_items WHERE workflow_id=?`, wfID).Scan(&seenCount); err != nil {
		t.Fatalf("query seen count: %v", err)
	}
	if seenCount != 2 {
		t.Errorf("expected 2 seen items, got %d", seenCount)
	}

	// ─── 运行 2：同数据立即二次运行，全部去重命中 -> skipped ────────────────────────
	runID2 := h.executeWorkflow(context.Background(), wf, "manual")
	waitRunStatus(t, db, runID2, "ok")

	// 验证 run 状态标记为 skipped
	var skipped2 int
	if err := db.QueryRow(`SELECT skipped FROM workflow_runs WHERE id=?`, runID2).Scan(&skipped2); err != nil {
		t.Fatalf("query skipped for run2: %v", err)
	}
	if skipped2 != 1 {
		t.Errorf("expected run2 to be skipped=1, got %d", skipped2)
	}

	// 验证 LLM 与投递调用计数仍为 1（第二次被短路跳过，未执行）
	inferMock.mu.Lock()
	totalInfers := len(inferMock.records)
	inferMock.mu.Unlock()
	if totalInfers != 1 {
		t.Errorf("LLM calls should remain 1, got %d", totalInfers)
	}

	deliverer.mu.Lock()
	totalDelivers := len(deliverer.calls)
	deliverer.mu.Unlock()
	if totalDelivers != 1 {
		t.Errorf("deliver calls should remain 1, got %d", totalDelivers)
	}

	t.Logf("运行 1 & 2 验证通过：fake LLM 最终调用次数=%d, purpose=%s, run1 CtxTaskIDKey=%s",
		totalInfers, r1.purpose, r1.taskID)

	// ─── 运行 3：Tool 步骤报错，seen 状态不被提交 ──────────────────────────────
	toolExec.fn = func(ctx context.Context, name string, args []byte, taint types.TaintLevel) (*types.ToolResult, error) {
		return &types.ToolResult{Success: false, Error: "network connection reset"}, nil
	}
	wfFail := &workflow{ID: "wf-fail", Type: "dag", Name: "Fail WF"}
	_, _ = db.Exec(`INSERT INTO workflows (id, type, name, enabled) VALUES ('wf-fail', 'dag', 'Fail WF', 1)`)
	stepFailConfig, _ := json.Marshal(pipeline.StepSpec{
		Key:  "failing_feed",
		Kind: "tool",
		Tool: "feed_fetch",
		Args: json.RawMessage(`{}`),
	})
	_, _ = db.Exec(`INSERT INTO workflow_steps (id, workflow_id, seq, name, depends_on, step_key, kind, config_json)
		VALUES ('ws-fail-1', 'wf-fail', 0, 'Feed', '[]', 'failing_feed', 'tool', ?)`, string(stepFailConfig))

	runID3 := h.executeWorkflow(context.Background(), wfFail, "manual")
	waitRunStatus(t, db, runID3, "error")

	// C3：失败运行不得残留去重状态；C4：失败原因必须落到 error_msg 与 step_outputs。
	if _, ok := h.dedupStates.Load(runID3); ok {
		t.Errorf("dedupStates leaked entry for failed run %s", runID3)
	}
	var errMsg3, stepOutputs3 string
	if err := db.QueryRow(`SELECT error_msg, step_outputs FROM workflow_runs WHERE id=?`, runID3).Scan(&errMsg3, &stepOutputs3); err != nil {
		t.Fatalf("query run3: %v", err)
	}
	if !strings.Contains(errMsg3, "network connection reset") || !strings.Contains(stepOutputs3, "network connection reset") {
		t.Errorf("failure reason lost: error_msg=%q step_outputs=%q", errMsg3, stepOutputs3)
	}

	var seenFailCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM workflow_seen_items WHERE workflow_id='wf-fail'`).Scan(&seenFailCount)
	if seenFailCount != 0 {
		t.Errorf("failed run should not commit seen items, got %d", seenFailCount)
	}

	// ─── 运行 4：Admitter 拒绝准入 ───────────────────────────────────────────
	admitter.admit = false
	runID4 := h.executeWorkflow(context.Background(), wf, "manual")
	var skipped4 int
	var errMsg4 string
	var status4 string
	if err := db.QueryRow(`SELECT skipped, error_msg, status FROM workflow_runs WHERE id=?`, runID4).Scan(&skipped4, &errMsg4, &status4); err != nil {
		t.Fatalf("query run4: %v", err)
	}
	if skipped4 != 1 || errMsg4 != "resource_pressure" || status4 != "ok" {
		t.Errorf("admitter rejection mismatch: skipped=%d, errMsg=%s, status=%s", skipped4, errMsg4, status4)
	}
}
