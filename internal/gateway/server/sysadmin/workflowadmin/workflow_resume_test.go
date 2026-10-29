package workflowadmin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// newResumeTestDB 用 SSoT DDL 建库（events/tasks/checkpoints/workflows），列漂移时测试随之暴露。
func newResumeTestDB(t *testing.T) *sql.DB {
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

func resumeReq(runID string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest("POST", "/v1/workflows/runs/"+runID+"/resume", nil)
	req.SetPathValue("id", runID)
	return httptest.NewRecorder(), req
}

func newResumeAdmin(db *sql.DB) *WorkflowAdmin {
	return &WorkflowAdmin{DB: db, WorkflowRepo: repo.NewSQLiteWorkflowRepository(db)}
}

func TestResumeWorkflowRun_StateMachine(t *testing.T) {
	db := newResumeTestDB(t)
	h := newResumeAdmin(db)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO workflows(id,name,enabled) VALUES('wf-ok','a',1),('wf-off','b',0)`)
	mustExec(`INSERT INTO workflow_runs(id,workflow_id,status) VALUES
		('r-running','wf-ok','running'),('r-done','wf-ok','ok'),
		('r-orphan','no-such-wf','interrupted'),('r-off','wf-off','interrupted')`)

	cases := []struct {
		run  string
		want int
	}{
		{"missing", http.StatusNotFound},
		{"r-running", http.StatusConflict},
		{"r-done", http.StatusConflict},
		{"r-orphan", http.StatusConflict}, // 工作流已删除
		{"r-off", http.StatusConflict},    // 工作流已禁用
	}
	for _, c := range cases {
		w, req := resumeReq(c.run)
		h.HandleResumeWorkflowRun(w, req)
		if w.Code != c.want {
			t.Errorf("run %s: got %d want %d (%s)", c.run, w.Code, c.want, strings.TrimSpace(w.Body.String()))
		}
	}
	// 被拒绝的请求不得改动运行状态。
	var st string
	if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id='r-off'`).Scan(&st); err != nil || st != "interrupted" {
		t.Errorf("r-off 状态被误改: %q %v", st, err)
	}
}

// 无 Blackboard 时续跑请求本身合法（202），后台立即 fail-closed 收尾为 error；
// 同时验证 ResumeWorkflowRun 的清理语义：旧 error 步骤记录被丢弃、ok 记录保留、current_step 重算。
func TestResumeWorkflowRun_AcceptedAndCleansFailedOutputs(t *testing.T) {
	db := newResumeTestDB(t)
	h := newResumeAdmin(db)
	for _, q := range []string{
		`INSERT INTO workflows(id,name,enabled,last_run_status,last_run_error) VALUES('wf1','a',1,'error','boom')`,
		`INSERT INTO workflow_steps(id,workflow_id,seq,name) VALUES('s0','wf1',0,'one')`,
		`INSERT INTO workflow_runs(id,workflow_id,status,current_step,total_steps,error_msg,finished_at,step_outputs)
		 VALUES('r1','wf1','interrupted',2,1,'interrupted by restart','2026-01-01T00:00:00Z',
		 '[{"seq":0,"status":"ok","output_preview":"a"},{"seq":1,"status":"error","output_preview":"b"}]')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ok, err := h.WorkflowRepo.ResumeWorkflowRun(context.Background(), "r1", "now")
	if err != nil || !ok {
		t.Fatalf("resume: ok=%v err=%v", ok, err)
	}
	var status, outputs, finished string
	var cur int
	if err := db.QueryRow(`SELECT status, step_outputs, finished_at, current_step FROM workflow_runs WHERE id='r1'`).Scan(&status, &outputs, &finished, &cur); err != nil {
		t.Fatal(err)
	}
	if status != "running" || finished != "" || cur != 1 || strings.Contains(outputs, `"error"`) || !strings.Contains(outputs, `"output_preview":"a"`) {
		t.Errorf("续跑清理不符: status=%s finished=%q cur=%d outputs=%s", status, finished, cur, outputs)
	}
	var wfStatus, wfErr string
	if err := db.QueryRow(`SELECT last_run_status, last_run_error FROM workflows WHERE id='wf1'`).Scan(&wfStatus, &wfErr); err != nil {
		t.Fatal(err)
	}
	if wfStatus != "running" || wfErr != "" {
		t.Errorf("workflows 未置 running: %s / %q", wfStatus, wfErr)
	}
	// 并发保护：第二次条件 UPDATE 命中 0 行。
	if ok, err := h.WorkflowRepo.ResumeWorkflowRun(context.Background(), "r1", "now"); err != nil || ok {
		t.Errorf("重复续跑应被拒: ok=%v err=%v", ok, err)
	}

	// HTTP 路径：把 run 再置为 error 后经 handler 续跑 → 202；无 Blackboard 后台 fail-closed 收尾。
	if _, err := db.Exec(`UPDATE workflow_runs SET status='error' WHERE id='r1'`); err != nil {
		t.Fatal(err)
	}
	w, req := resumeReq("r1")
	h.HandleResumeWorkflowRun(w, req)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"run_id":"r1"`) {
		t.Fatalf("want 202 with run_id, got %d %s", w.Code, w.Body.String())
	}
	waitRunStatus(t, db, "r1", "error")
}

func waitRunStatus(t *testing.T, db *sql.DB, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id=?`, runID).Scan(&st); err == nil && st == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s 未在期限内进入 %s", runID, want)
}

// 续跑复用 done checkpoint：step0 已 done，只应为 step1 投递任务，不得为 step0 重投。
func TestResumeWorkflowRun_ReusesDoneCheckpoint(t *testing.T) {
	db := newResumeTestDB(t)
	bb := orchestrator.NewSQLiteBlackboard(db)
	h := newResumeAdmin(db)
	h.Blackboard = bb
	for _, q := range []string{
		`INSERT INTO workflows(id,name,enabled,last_run_status) VALUES('wf1','a',1,'error')`,
		`INSERT INTO workflow_steps(id,workflow_id,seq,name) VALUES('s0','wf1',0,'one'),('s1','wf1',1,'two')`,
		`INSERT INTO workflow_runs(id,workflow_id,status,total_steps) VALUES('r1','wf1','interrupted',2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := repo.NewSQLiteTaskCheckpointRepository(db).UpsertCheckpoint(context.Background(), types.TaskCheckpointRow{
		TaskID: "r1", NodeID: "s0", Attempt: 1, Status: "done", OutputJSON: `{"reply":"x"}`, CompletedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	w, req := resumeReq("r1")
	h.HandleResumeWorkflowRun(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", w.Code, w.Body.String())
	}

	var taskID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT task_id FROM tasks WHERE task_id LIKE 'r1-s1-%'`).Scan(&taskID); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if taskID == "" {
		t.Fatal("step1 任务未投递")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE task_id LIKE 'r1-s0-%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("done 节点不应重投任务: n=%d err=%v", n, err)
	}

	// 模拟 Worker 完成 step1，图收尾后 run 应落 ok。
	ctx := context.Background()
	if claimed, err := bb.ClaimTask(ctx, taskID, "tester"); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := bb.CompleteTask(ctx, taskID, "tester", []byte(`{"reply":"y"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	waitRunStatus(t, db, "r1", "ok")
}
