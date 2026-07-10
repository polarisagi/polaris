package optimizer

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// newRolloutTestDB 创建内存 SQLite，同时建好 rollout_states（由 NewSQLiteRolloutStore
// 自建）和 prompt_versions（手工建表，与 010_self_improve.sql 定义对齐）两张表，
// 供 ConfirmShadow → promptActivator.Activate 的联动测试使用。
func newRolloutTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE prompt_versions (
			id TEXT PRIMARY KEY,
			version INTEGER,
			task_type TEXT,
			prompt_text TEXT,
			score REAL,
			cost REAL,
			source TEXT,
			parent_version TEXT,
			is_active INTEGER,
			created_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("failed to create prompt_versions: %v", err)
	}
	return db
}

// TestConfirmShadow_ActivatesPromptCandidate 验证 ConfirmShadow 通过后，若注入了
// promptActivator，会用 SubmitCandidate 时落盘的 TaskType/BaselineScore 回调激活
// 对应的 Prompt 候选——这是本次修复 ADR-0029 §K"Gate 2/3 被 Activate() 同步旁路"
// 问题的核心断言。
func TestConfirmShadow_ActivatesPromptCandidate(t *testing.T) {
	db := newRolloutTestDB(t)
	ctx := context.Background()

	versionStore := NewPromptVersionStore(db)
	if err := versionStore.Save(ctx, &PromptVersion{
		ID: "cand-1", Version: 2, TaskType: "chat", Prompt: "new prompt text", Score: 0.9,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	rolloutStore, err := NewSQLiteRolloutStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteRolloutStore: %v", err)
	}
	rolloutStore.WithPromptActivator(versionStore)

	if err := rolloutStore.SubmitCandidate(ctx, &AgentVersionSnapshot{
		Version:       "cand-1",
		TaskType:      "chat",
		BaselineScore: 0.8,
	}); err != nil {
		t.Fatalf("SubmitCandidate: %v", err)
	}

	if err := rolloutStore.ConfirmShadow(ctx, "cand-1"); err != nil {
		t.Fatalf("ConfirmShadow: %v", err)
	}

	active, err := versionStore.GetActive(ctx, "chat")
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if active == nil {
		t.Fatal("expected candidate to be activated after ConfirmShadow, got none active")
	}
	if active.ID != "cand-1" {
		t.Errorf("expected active ID=cand-1, got %s", active.ID)
	}

	state, err := rolloutStore.GetState(ctx, "cand-1")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.CurrentGate != GateCanaryRollout {
		t.Errorf("expected CurrentGate=GateCanaryRollout(2) after ConfirmShadow, got %d", state.CurrentGate)
	}
}

// TestConfirmShadow_NoActivatorSkipsActivation 验证未注入 promptActivator（纯 L3/L4
// 候选场景）时，ConfirmShadow 仍能正常推进 Gate，不 panic、不报错。
func TestConfirmShadow_NoActivatorSkipsActivation(t *testing.T) {
	db := newRolloutTestDB(t)
	ctx := context.Background()

	rolloutStore, err := NewSQLiteRolloutStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteRolloutStore: %v", err)
	}

	if err := rolloutStore.SubmitCandidate(ctx, &AgentVersionSnapshot{Version: "cand-l3"}); err != nil {
		t.Fatalf("SubmitCandidate: %v", err)
	}
	if err := rolloutStore.ConfirmShadow(ctx, "cand-l3"); err != nil {
		t.Fatalf("ConfirmShadow: %v", err)
	}

	state, err := rolloutStore.GetState(ctx, "cand-l3")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.CurrentGate != GateCanaryRollout {
		t.Errorf("expected CurrentGate=GateCanaryRollout(2), got %d", state.CurrentGate)
	}
}

// TestListPendingShadow 验证只返回停留在 Gate 2(Shadow)、status=running 的候选，
// 已推进到 Canary 或已回滚的候选不应出现。
func TestListPendingShadow(t *testing.T) {
	db := newRolloutTestDB(t)
	ctx := context.Background()

	rolloutStore, err := NewSQLiteRolloutStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteRolloutStore: %v", err)
	}

	for _, v := range []string{"pending-1", "pending-2", "advanced-1", "rolled-back-1"} {
		if err := rolloutStore.SubmitCandidate(ctx, &AgentVersionSnapshot{Version: v}); err != nil {
			t.Fatalf("SubmitCandidate(%s): %v", v, err)
		}
		// RecordEvalScore 将 status 从 pending 推进为 running——ListPendingShadow
		// 只关心 running 中的候选（pending 尚未过 Gate 1 Eval 阈值判定）。
		if err := rolloutStore.RecordEvalScore(ctx, v, 0.9, 0.8); err != nil {
			t.Fatalf("RecordEvalScore(%s): %v", v, err)
		}
	}
	if err := rolloutStore.ConfirmShadow(ctx, "advanced-1"); err != nil {
		t.Fatalf("ConfirmShadow(advanced-1): %v", err)
	}
	if err := rolloutStore.Rollback(ctx, "rolled-back-1", "test rollback"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	versions, err := rolloutStore.ListPendingShadow(ctx)
	if err != nil {
		t.Fatalf("ListPendingShadow: %v", err)
	}

	got := map[string]bool{}
	for _, v := range versions {
		got[v] = true
	}
	if !got["pending-1"] || !got["pending-2"] {
		t.Errorf("expected pending-1 and pending-2 in result, got %v", versions)
	}
	if got["advanced-1"] {
		t.Errorf("advanced-1 already past Gate 2, should not be listed: %v", versions)
	}
	if got["rolled-back-1"] {
		t.Errorf("rolled-back-1 is rolled back, should not be listed: %v", versions)
	}
}
