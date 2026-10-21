package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newGoalTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ddl, err := schema.FS.ReadFile("053_agent_goals.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestGoalRepository_SetReplacesActive(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteGoalRepository(newGoalTestDB(t))
	g1, err := r.SetGoal(ctx, "s1", "a", "", 0)
	if err != nil || g1.MaxRounds != 20 {
		t.Fatalf("%v %+v", err, g1)
	}
	g2, _ := r.SetGoal(ctx, "s1", "b", "done", 3)
	act, _ := r.ActiveGoal(ctx, "s1")
	if act == nil || act.ID != g2.ID || act.MaxRounds != 3 {
		t.Fatalf("active=%+v", act)
	}
	old, _ := r.byID(ctx, g1.ID)
	if old.Status != repo.GoalPaused {
		t.Fatalf("旧目标应 paused: %s", old.Status)
	}
	if other, _ := r.ActiveGoal(ctx, "s2"); other != nil {
		t.Fatal("会话隔离")
	}
}

func TestGoalRepository_RoundsAndIdleAutoPause(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteGoalRepository(newGoalTestDB(t))
	g, _ := r.SetGoal(ctx, "s1", "a", "", 5)
	for i := 1; i <= 2; i++ {
		got, err := r.BeginRound(ctx, g.ID)
		if err != nil || got == nil || got.RoundsUsed != i {
			t.Fatalf("round %d: %v %+v", i, err, got)
		}
	}
	got, _ := r.FinishRound(ctx, g.ID, true, 2)
	if got.IdleRounds != 1 || got.Status != repo.GoalActive {
		t.Fatalf("%+v", got)
	}
	got, _ = r.FinishRound(ctx, g.ID, false, 2)
	if got.IdleRounds != 0 {
		t.Fatalf("有工具调用应清零: %+v", got)
	}
	_, _ = r.FinishRound(ctx, g.ID, true, 2)
	got, _ = r.FinishRound(ctx, g.ID, true, 2)
	if got.Status != repo.GoalPaused {
		t.Fatalf("连续两次空转应 paused: %+v", got)
	}
	if again, _ := r.BeginRound(ctx, g.ID); again != nil {
		t.Fatal("非 active 目标不应再登记续跑")
	}
}

func TestGoalRepository_UpdatePauseCurrent(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteGoalRepository(newGoalTestDB(t))
	if err := r.UpdateGoal(ctx, "s1", repo.GoalComplete, ""); err == nil {
		t.Fatal("无目标应报错")
	}
	_, _ = r.SetGoal(ctx, "s1", "a", "", 0)
	if err := r.UpdateGoal(ctx, "s1", repo.GoalBlocked, "need key"); err != nil {
		t.Fatal(err)
	}
	cur, _ := r.CurrentGoal(ctx, "s1")
	if cur.Status != repo.GoalBlocked || cur.Note != "need key" {
		t.Fatalf("%+v", cur)
	}
	if act, _ := r.ActiveGoal(ctx, "s1"); act != nil {
		t.Fatal("blocked 不应是 active")
	}
	_ = r.UpdateGoal(ctx, "s1", repo.GoalActive, "")
	if err := r.PauseGoal(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	cur, _ = r.CurrentGoal(ctx, "s1")
	if cur.Status != repo.GoalPaused {
		t.Fatalf("%+v", cur)
	}
}
