package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newOrphanTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, f := range []string{"017_automations.sql", "029_workflows.sql", "031_planner_sessions.sql", "042_subagent_runs.sql"} {
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

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func scalar[T any](t *testing.T, db *sql.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func TestReconcileOrphanRuns(t *testing.T) {
	db := newOrphanTestDB(t)
	ctx := context.Background()

	mustExec(t, db, `INSERT INTO automations(id,name,prompt,last_run_status,failure_count,circuit_open) VALUES('a1','x','p','running',2,0),('a2','y','p','ok',0,0)`)
	mustExec(t, db, `INSERT INTO automation_runs(id,automation_id,status) VALUES('ar1','a1','running'),('ar2','a2','ok')`)
	mustExec(t, db, `INSERT INTO workflows(id,name,last_run_status,failure_count) VALUES('w1','x','running',1),('w2','y','error',0)`)
	mustExec(t, db, `INSERT INTO workflow_runs(id,workflow_id,status) VALUES('wr1','w1','running'),('wr2','w2','error')`)
	mustExec(t, db, `INSERT INTO subagent_runs(id,entry,status,started_at) VALUES('s1','hook','running',1),('s2','hook','ok',1)`)
	mustExec(t, db, `INSERT INTO planner_sessions(id,goal,task_type,status,created_at) VALUES('p1','g','general','running',1),('p2','g','general','done',1)`)

	now := time.UnixMilli(1_800_000_000_000)
	if err := reconcileOrphanRuns(ctx, db, now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := scalar[string](t, db, `SELECT status FROM automation_runs WHERE id='ar1'`); got != "interrupted" {
		t.Errorf("automation_run: %s", got)
	}
	if got := scalar[string](t, db, `SELECT error_msg FROM automation_runs WHERE id='ar1'`); got != interruptedByRestart {
		t.Errorf("automation_run error_msg: %s", got)
	}
	if got := scalar[string](t, db, `SELECT finished_at FROM automation_runs WHERE id='ar1'`); got != now.UTC().Format(time.RFC3339) {
		t.Errorf("automation_run finished_at: %s", got)
	}
	if got := scalar[string](t, db, `SELECT last_run_status FROM automations WHERE id='a1'`); got != "error" {
		t.Errorf("automation last_run_status: %s", got)
	}
	if got := scalar[int](t, db, `SELECT failure_count FROM automations WHERE id='a1'`); got != 2 {
		t.Errorf("automation failure_count 不应变化: %d", got)
	}
	if got := scalar[int](t, db, `SELECT circuit_open FROM automations WHERE id='a1'`); got != 0 {
		t.Errorf("automation circuit_open 不应变化: %d", got)
	}
	if got := scalar[string](t, db, `SELECT status FROM workflow_runs WHERE id='wr1'`); got != "interrupted" {
		t.Errorf("workflow_run: %s", got)
	}
	if got := scalar[string](t, db, `SELECT last_run_error FROM workflows WHERE id='w1'`); got != interruptedByRestart {
		t.Errorf("workflow last_run_error: %s", got)
	}
	if got := scalar[int](t, db, `SELECT failure_count FROM workflows WHERE id='w1'`); got != 1 {
		t.Errorf("workflow failure_count 不应变化: %d", got)
	}
	if got := scalar[string](t, db, `SELECT status FROM subagent_runs WHERE id='s1'`); got != "interrupted" {
		t.Errorf("subagent_run: %s", got)
	}
	if got := scalar[int64](t, db, `SELECT finished_at FROM subagent_runs WHERE id='s1'`); got != now.UnixMilli() {
		t.Errorf("subagent_run finished_at: %d", got)
	}
	if got := scalar[string](t, db, `SELECT status FROM planner_sessions WHERE id='p1'`); got != "failed" {
		t.Errorf("planner_session: %s", got)
	}

	// 非 running 行不受影响。
	if got := scalar[string](t, db, `SELECT status FROM automation_runs WHERE id='ar2'`); got != "ok" {
		t.Errorf("ar2 被误改: %s", got)
	}
	if got := scalar[string](t, db, `SELECT last_run_status FROM automations WHERE id='a2'`); got != "ok" {
		t.Errorf("a2 被误改: %s", got)
	}
	if got := scalar[string](t, db, `SELECT last_run_error FROM workflows WHERE id='w2'`); got != "" {
		t.Errorf("w2 被误改: %s", got)
	}
	if got := scalar[string](t, db, `SELECT status FROM subagent_runs WHERE id='s2'`); got != "ok" {
		t.Errorf("s2 被误改: %s", got)
	}
	if got := scalar[string](t, db, `SELECT status FROM planner_sessions WHERE id='p2'`); got != "done" {
		t.Errorf("p2 被误改: %s", got)
	}

	// 幂等：再次对账无变化。
	if err := reconcileOrphanRuns(ctx, db, now.Add(time.Hour)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := scalar[int64](t, db, `SELECT finished_at FROM subagent_runs WHERE id='s1'`); got != now.UnixMilli() {
		t.Errorf("二次对账改写了已终态行: %d", got)
	}
}

// 任一语句失败整体回滚：缺表时不得留下部分更新。
func TestReconcileOrphanRuns_RollbackOnError(t *testing.T) {
	db := newOrphanTestDB(t)
	mustExec(t, db, `INSERT INTO automation_runs(id,automation_id,status) VALUES('ar1','a1','running')`)
	mustExec(t, db, `DROP TABLE planner_sessions`)
	if err := reconcileOrphanRuns(context.Background(), db, time.Now()); err == nil {
		t.Fatal("缺表应返回错误")
	}
	if got := scalar[string](t, db, `SELECT status FROM automation_runs WHERE id='ar1'`); got != "running" {
		t.Errorf("失败应整体回滚，got %s", got)
	}
}
