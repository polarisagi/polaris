package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newRunRecordsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, f := range []string{"031_planner_sessions.sql", "042_subagent_runs.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

func TestSubagentRunRepository_LifecycleAndUpsert(t *testing.T) {
	db := newRunRecordsDB(t)
	r := NewSQLiteSubagentRunRepository(db)
	ctx := context.Background()

	row := repo.SubagentRunRow{ID: "a1", ParentSessionID: "p", ChildSessionID: "sub-a1", TaskID: "t1",
		AgentType: "x", Entry: "delegation", Prompt: "hi", StartedAtMs: 1}
	if err := r.Start(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "a1", "ok", "out", "", 2); err != nil {
		t.Fatal(err)
	}
	var status, out string
	var cont int
	var fin sql.NullInt64
	if err := db.QueryRow(`SELECT status, output, continuations, finished_at FROM subagent_runs WHERE id='a1'`).Scan(&status, &out, &cont, &fin); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || out != "out" || cont != 2 || !fin.Valid {
		t.Errorf("finish 未落库: %s %s %d %v", status, out, cont, fin)
	}

	// 委派任务重试复用同一 id：重置回 running，不因主键冲突丢记录。
	if err := r.Start(ctx, row); err != nil {
		t.Fatalf("重复 Start 应 upsert: %v", err)
	}
	if err := db.QueryRow(`SELECT status, output, continuations, finished_at FROM subagent_runs WHERE id='a1'`).Scan(&status, &out, &cont, &fin); err != nil {
		t.Fatal(err)
	}
	if status != "running" || out != "" || cont != 0 || fin.Valid {
		t.Errorf("upsert 未重置: %s %q %d %v", status, out, cont, fin)
	}

	// 非委派入口 task_id 存 NULL。
	if err := r.Start(ctx, repo.SubagentRunRow{ID: "a2", Entry: "hook", StartedAtMs: 1}); err != nil {
		t.Fatal(err)
	}
	var tid sql.NullString
	if err := db.QueryRow(`SELECT task_id FROM subagent_runs WHERE id='a2'`).Scan(&tid); err != nil || tid.Valid {
		t.Errorf("task_id 应为 NULL: %v %v", tid, err)
	}
	// 非法 entry 被 CHECK 拒绝。
	if err := r.Start(ctx, repo.SubagentRunRow{ID: "a3", Entry: "bogus", StartedAtMs: 1}); err == nil {
		t.Error("非法 entry 应被拒绝")
	}
}

func TestPlannerSessionRepository_Lifecycle(t *testing.T) {
	db := newRunRecordsDB(t)
	r := NewSQLitePlannerSessionRepository(db)
	ctx := context.Background()
	if err := r.Start(ctx, "plan_1", "task", "goal", "general", 3); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM planner_sessions WHERE id='plan_1'`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("start: %s %v", status, err)
	}
	if err := r.Finish(ctx, "plan_1", "done", 0.9, "engine_b"); err != nil {
		t.Fatal(err)
	}
	var score float64
	var engine string
	var completed sql.NullInt64
	if err := db.QueryRow(`SELECT status, winning_score, winning_engine, completed_at FROM planner_sessions WHERE id='plan_1'`).Scan(&status, &score, &engine, &completed); err != nil {
		t.Fatal(err)
	}
	if status != "done" || score != 0.9 || engine != "engine_b" || !completed.Valid {
		t.Errorf("finish: %s %v %s %v", status, score, engine, completed)
	}
	if err := r.Start(ctx, "plan_1", "", "g", "general", 3); err == nil {
		t.Error("重复 id 应主键冲突")
	}
}
