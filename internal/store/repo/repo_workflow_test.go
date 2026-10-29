package repo_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
)

func newWorkflowTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	for _, file := range []string{"029_workflows.sql", "039_llm_calls.sql"} {
		ddl, err := schema.FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("exec %s: %v", file, err)
		}
	}
	return db
}

func TestWorkflowRepository_CRUDWithPipelineFields(t *testing.T) {
	ctx := context.Background()
	db := newWorkflowTestDB(t)
	r := storerepo.NewSQLiteWorkflowRepository(db)

	wf := repo.WorkflowRow{
		ID:           "wf_test_01",
		Type:         "dag",
		Name:         "Pipeline Workflow",
		Description:  "Workflow with tool and transform steps",
		TriggerType:  "cron",
		CronSchedule: "0 * * * *",
		Enabled:      true,
		ResultAction: "channel:general",
		DeliverFrom:  "digest",
		Source:       "user",
		NextRunAt:    "2026-10-14T16:00:00Z",
		CreatedAt:    "2026-10-14T15:00:00Z",
		UpdatedAt:    "2026-10-14T15:00:00Z",
	}

	steps := []repo.WorkflowStepRow{
		{
			ID:              "ws_step_01",
			WorkflowID:      "wf_test_01",
			Seq:             0,
			Name:            "Fetch Feed",
			StepKey:         "fetch",
			Kind:            "tool",
			ConfigJSON:      `{"tool":"feed_fetch"}`,
			ReasoningEffort: "medium",
		},
		{
			ID:              "ws_step_02",
			WorkflowID:      "wf_test_01",
			Seq:             1,
			Name:            "Digest Items",
			StepKey:         "digest",
			Kind:            "llm",
			ConfigJSON:      `{"prompt":"Summarize"}`,
			ReasoningEffort: "medium",
		},
	}

	if err := r.CreateWorkflowWithSteps(ctx, wf, steps); err != nil {
		t.Fatalf("CreateWorkflowWithSteps: %v", err)
	}

	// 验证库内字段正确落盘
	var resultAction, deliverFrom, source string
	err := db.QueryRowContext(ctx, "SELECT result_action, deliver_from, source FROM workflows WHERE id=?", wf.ID).
		Scan(&resultAction, &deliverFrom, &source)
	if err != nil {
		t.Fatalf("scan workflows: %v", err)
	}
	if resultAction != "channel:general" || deliverFrom != "digest" || source != "user" {
		t.Errorf("unexpected workflows fields: action=%s from=%s src=%s", resultAction, deliverFrom, source)
	}

	var stepKey, kind, configJSON string
	err = db.QueryRowContext(ctx, "SELECT step_key, kind, config_json FROM workflow_steps WHERE id=?", "ws_step_01").
		Scan(&stepKey, &kind, &configJSON)
	if err != nil {
		t.Fatalf("scan workflow_steps: %v", err)
	}
	if stepKey != "fetch" || kind != "tool" || configJSON != `{"tool":"feed_fetch"}` {
		t.Errorf("unexpected step fields: key=%s kind=%s cfg=%s", stepKey, kind, configJSON)
	}

	// 更新工作流
	wf.ResultAction = "silent"
	wf.DeliverFrom = "other"
	steps[0].Kind = "transform"
	if err := r.UpdateWorkflowWithSteps(ctx, wf, steps, true); err != nil {
		t.Fatalf("UpdateWorkflowWithSteps: %v", err)
	}

	err = db.QueryRowContext(ctx, "SELECT result_action, deliver_from FROM workflows WHERE id=?", wf.ID).
		Scan(&resultAction, &deliverFrom)
	if err != nil {
		t.Fatalf("scan updated workflows: %v", err)
	}
	if resultAction != "silent" || deliverFrom != "other" {
		t.Errorf("unexpected updated fields: action=%s from=%s", resultAction, deliverFrom)
	}
}

func TestWorkflowRepository_RunData(t *testing.T) {
	ctx := context.Background()
	db := newWorkflowTestDB(t)
	r := storerepo.NewSQLiteWorkflowRepository(db)

	// 1. 读取不存在的数据返回空
	status, data, err := r.GetRunData(ctx, "wfr_nonexistent", "step_01")
	if err != nil {
		t.Fatalf("GetRunData nonexistent: %v", err)
	}
	if status != "" || data != nil {
		t.Fatalf("expected empty status and nil data, got status=%q data=%v", status, data)
	}

	// 2. 写入并读取
	dataJSON := []byte(`{"items":[{"title":"Item 1"}],"count":1}`)
	if err := r.SaveRunData(ctx, "wfr_01", "step_01", "ok", dataJSON, 1); err != nil {
		t.Fatalf("SaveRunData: %v", err)
	}

	status, data, err = r.GetRunData(ctx, "wfr_01", "step_01")
	if err != nil {
		t.Fatalf("GetRunData: %v", err)
	}
	if status != "ok" || string(data) != string(dataJSON) {
		t.Fatalf("unexpected loaded run data: status=%s data=%s", status, string(data))
	}

	// 3. 覆盖写入
	updatedJSON := []byte(`{"items":[],"count":0}`)
	if err := r.SaveRunData(ctx, "wfr_01", "step_01", "skipped", updatedJSON, 0); err != nil {
		t.Fatalf("SaveRunData overwrite: %v", err)
	}
	status, data, _ = r.GetRunData(ctx, "wfr_01", "step_01")
	if status != "skipped" || string(data) != string(updatedJSON) {
		t.Fatalf("expected overwritten status skipped, got %s", status)
	}
}

func TestWorkflowRepository_SeenKeys(t *testing.T) {
	ctx := context.Background()
	db := newWorkflowTestDB(t)
	r := storerepo.NewSQLiteWorkflowRepository(db)

	wfID := "wf_seen_test"
	keys := []string{"k1", "k2", "k3"}

	// 初始查询无命中
	seen, err := r.SeenKeys(ctx, wfID, keys)
	if err != nil {
		t.Fatalf("SeenKeys: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("expected empty seen, got %d", len(seen))
	}

	// 标记 k1, k2
	if err := r.MarkSeen(ctx, wfID, []string{"k1", "k2"}, "2026-10-14T10:00:00Z"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	// 再次查询
	seen, err = r.SeenKeys(ctx, wfID, keys)
	if err != nil {
		t.Fatalf("SeenKeys after mark: %v", err)
	}
	if !seen["k1"] || !seen["k2"] || seen["k3"] {
		t.Fatalf("unexpected seen map: %+v", seen)
	}

	// 重复标记不报错 (INSERT OR IGNORE)
	if err := r.MarkSeen(ctx, wfID, []string{"k1"}, "2026-10-14T11:00:00Z"); err != nil {
		t.Fatalf("MarkSeen duplicate: %v", err)
	}
}

func TestWorkflowRepository_LastSuccessfulRunStartAndSetSkipped(t *testing.T) {
	ctx := context.Background()
	db := newWorkflowTestDB(t)
	r := storerepo.NewSQLiteWorkflowRepository(db)

	wfID := "wf_runs_test"

	// 初始无成功运行
	start, err := r.LastSuccessfulRunStart(ctx, wfID)
	if err != nil {
		t.Fatalf("LastSuccessfulRunStart: %v", err)
	}
	if start != "" {
		t.Fatalf("expected empty start, got %q", start)
	}

	// 插入不同状态运行
	if err := r.CreateWorkflowRun(ctx, "wfr_err", wfID, "cron", "error", 1, 2, "2026-10-14T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkflowRun(ctx, "wfr_ok_1", wfID, "cron", "ok", 2, 2, "2026-10-14T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkflowRun(ctx, "wfr_ok_2", wfID, "cron", "ok", 2, 2, "2026-10-14T12:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// 将最新的 wfr_ok_2 标记为 skipped
	if err := r.SetRunSkipped(ctx, "wfr_ok_2"); err != nil {
		t.Fatalf("SetRunSkipped: %v", err)
	}

	// 校验 LastSuccessfulRunStart 忽略 skipped=1，命中 11:00:00Z 的 wfr_ok_1
	start, err = r.LastSuccessfulRunStart(ctx, wfID)
	if err != nil {
		t.Fatalf("LastSuccessfulRunStart: %v", err)
	}
	if start != "2026-10-14T11:00:00Z" {
		t.Fatalf("expected 2026-10-14T11:00:00Z, got %q", start)
	}
}

func TestWorkflowRepository_PruneRunDataAndSeen(t *testing.T) {
	ctx := context.Background()
	db := newWorkflowTestDB(t)
	r := storerepo.NewSQLiteWorkflowRepository(db)

	wfID := "wf_prune_test"

	// 插入 3 次运行与对应 run_data
	runs := []struct {
		id   string
		time string
	}{
		{"wfr_p1", "2026-10-14T08:00:00Z"},
		{"wfr_p2", "2026-10-14T09:00:00Z"},
		{"wfr_p3", "2026-10-14T10:00:00Z"},
	}
	for _, run := range runs {
		_ = r.CreateWorkflowRun(ctx, run.id, wfID, "cron", "ok", 1, 1, run.time)
		_ = r.SaveRunData(ctx, run.id, "step", "ok", []byte(`{}`), 0)
	}

	// 保留最近 2 次，wfr_p1 的数据应被删除
	if err := r.PruneRunData(ctx, wfID, 2); err != nil {
		t.Fatalf("PruneRunData: %v", err)
	}

	status1, _, _ := r.GetRunData(ctx, "wfr_p1", "step")
	if status1 != "" {
		t.Fatalf("expected wfr_p1 data to be pruned, got status=%q", status1)
	}
	status2, _, _ := r.GetRunData(ctx, "wfr_p2", "step")
	if status2 != "ok" {
		t.Fatalf("expected wfr_p2 data to remain")
	}

	// 测试 PruneSeen
	_ = r.MarkSeen(ctx, wfID, []string{"old_k"}, "2026-10-10T00:00:00Z")
	_ = r.MarkSeen(ctx, wfID, []string{"new_k"}, "2026-10-14T00:00:00Z")
	if err := r.PruneSeen(ctx, "2026-10-12T00:00:00Z"); err != nil {
		t.Fatalf("PruneSeen: %v", err)
	}
	seen, _ := r.SeenKeys(ctx, wfID, []string{"old_k", "new_k"})
	if seen["old_k"] || !seen["new_k"] {
		t.Fatalf("unexpected seen after prune: %+v", seen)
	}
}

func TestWorkflowRepository_RunLLMUsage(t *testing.T) {
	ctx := context.Background()
	db := newWorkflowTestDB(t)
	r := storerepo.NewSQLiteWorkflowRepository(db)

	now := time.Now().UnixMilli()
	// 插入两条属于 session_1 和 session_2 的 llm_calls 记录
	_, err := db.ExecContext(ctx, `
		INSERT INTO llm_calls (id, created_at, session_id, provider, status, input_tokens, output_tokens, cost_usd)
		VALUES
			('call_1', ?, 'session_1', 'openai', 'ok', 100, 50, 0.002),
			('call_2', ?, 'session_2', 'openai', 'ok', 200, 80, 0.005),
			('call_3', ?, 'other_sess', 'openai', 'ok', 500, 200, 0.010)`,
		now, now, now,
	)
	if err != nil {
		t.Fatalf("insert llm_calls: %v", err)
	}

	inTok, outTok, cost, err := r.RunLLMUsage(ctx, []string{"session_1", "session_2"})
	if err != nil {
		t.Fatalf("RunLLMUsage: %v", err)
	}
	if inTok != 300 || outTok != 130 || cost < 0.0069 || cost > 0.0071 {
		t.Fatalf("unexpected usage: in=%d out=%d cost=%f", inTok, outTok, cost)
	}

	// 空 session 返回 0
	inTok, outTok, cost, err = r.RunLLMUsage(ctx, nil)
	if err != nil || inTok != 0 || outTok != 0 || cost != 0 {
		t.Fatalf("expected 0 usage for nil sessions")
	}
}
