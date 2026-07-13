package optimizer

import (
	"context"
	"database/sql"
	"testing"
	"time"

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

// ── V8-S2 Meta-Eval 前置检查（WithMetaAudit / AdvanceGate）──────────────────────

// fakeMetaAuditReader 是测试专用的 MetaAuditReader 实现，可编排任意返回值组合。
type fakeMetaAuditReader struct {
	passed     bool
	computedAt time.Time
	ok         bool
	err        error
}

func (f *fakeMetaAuditReader) LatestMetaAudit(context.Context) (bool, time.Time, bool, error) {
	return f.passed, f.computedAt, f.ok, f.err
}

// backdateLastAdvanced 直接改写 last_advanced_at，绕开真实等待，模拟"24h 稳定期已过"。
func backdateLastAdvanced(t *testing.T, db *sql.DB, version string, ago time.Duration) {
	t.Helper()
	past := time.Now().Add(-ago).Unix()
	if _, err := db.Exec(`UPDATE rollout_states SET last_advanced_at = ? WHERE version = ?`, past, version); err != nil {
		t.Fatalf("backdateLastAdvanced: %v", err)
	}
}

// advanceToCanaryGate2 让候选一路推进到 Gate2(Shadow 确认后的 Canary 5%)，
// 供后续 AdvanceGate 测试作为共同起点。
func advanceToCanaryGate2(t *testing.T, ctx context.Context, rs *SQLiteRolloutStore, version string) {
	t.Helper()
	if err := rs.SubmitCandidate(ctx, &AgentVersionSnapshot{Version: version}); err != nil {
		t.Fatalf("SubmitCandidate: %v", err)
	}
	if err := rs.ConfirmShadow(ctx, version); err != nil {
		t.Fatalf("ConfirmShadow: %v", err)
	}
}

// TestAdvanceGate_MetaAuditDisabled_SkipsCheckEntirely 验证默认关闭
// （MetaAuditGateEnabled=false，或未调用 WithMetaAudit）时 AdvanceGate 完全不受
// meta_audit 结论影响——这是配置默认值的核心保证：不应因为新功能而破坏既有部署。
func TestAdvanceGate_MetaAuditDisabled_SkipsCheckEntirely(t *testing.T) {
	db := newRolloutTestDB(t)
	ctx := context.Background()
	rs, err := NewSQLiteRolloutStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteRolloutStore: %v", err)
	}
	advanceToCanaryGate2(t, ctx, rs, "v-disabled")
	backdateLastAdvanced(t, db, "v-disabled", 25*time.Hour)

	// 未调用 WithMetaAudit：metaAuditReader 为 nil，metaAuditGateEnabled 为 false zero value。
	state, err := rs.AdvanceGate(ctx, "v-disabled", RolloutStats{})
	if err != nil {
		t.Fatalf("AdvanceGate: %v", err)
	}
	if state.CurrentGate != GateCanaryRollout+1 {
		t.Errorf("expected gate to advance past Gate2 when meta_audit gating disabled, got gate=%d", state.CurrentGate)
	}
}

// TestAdvanceGate_MetaAuditEnabled_HoldsWhenMissingFailedOrStale 验证启用后，
// 结论缺失/未通过/过期三种情况均 fail-closed：停在当前 Gate，不推进、不回滚。
func TestAdvanceGate_MetaAuditEnabled_HoldsWhenMissingFailedOrStale(t *testing.T) {
	cases := []struct {
		name   string
		reader *fakeMetaAuditReader
	}{
		{"never_audited", &fakeMetaAuditReader{ok: false}},
		{"audited_but_failed", &fakeMetaAuditReader{ok: true, passed: false, computedAt: time.Now()}},
		{"audited_passed_but_stale", &fakeMetaAuditReader{ok: true, passed: true, computedAt: time.Now().Add(-200 * time.Hour)}},
		{"reader_errored", &fakeMetaAuditReader{err: context.DeadlineExceeded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newRolloutTestDB(t)
			ctx := context.Background()
			rs, err := NewSQLiteRolloutStore(db)
			if err != nil {
				t.Fatalf("NewSQLiteRolloutStore: %v", err)
			}
			rs.WithMetaAudit(tc.reader, 168*time.Hour, true)
			advanceToCanaryGate2(t, ctx, rs, "v-hold")
			backdateLastAdvanced(t, db, "v-hold", 25*time.Hour)

			state, err := rs.AdvanceGate(ctx, "v-hold", RolloutStats{})
			if err != nil {
				t.Fatalf("AdvanceGate: %v", err)
			}
			if state.CurrentGate != GateCanaryRollout {
				t.Errorf("expected gate to stay at GateCanaryRollout(2) (fail-closed), got gate=%d", state.CurrentGate)
			}
			if state.Status == RolloutStatusRolledBack {
				t.Error("meta_audit failure must not trigger Rollback (may just be unaudited yet, not a candidate defect)")
			}
		})
	}
}

// TestAdvanceGate_MetaAuditEnabled_AdvancesWhenFreshAndPassed 验证启用后，
// 结论存在、通过、且在新鲜度窗口内时，Gate 正常按 canarySteps 推进。
func TestAdvanceGate_MetaAuditEnabled_AdvancesWhenFreshAndPassed(t *testing.T) {
	db := newRolloutTestDB(t)
	ctx := context.Background()
	rs, err := NewSQLiteRolloutStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteRolloutStore: %v", err)
	}
	reader := &fakeMetaAuditReader{ok: true, passed: true, computedAt: time.Now()}
	rs.WithMetaAudit(reader, 168*time.Hour, true)
	advanceToCanaryGate2(t, ctx, rs, "v-fresh")
	backdateLastAdvanced(t, db, "v-fresh", 25*time.Hour)

	state, err := rs.AdvanceGate(ctx, "v-fresh", RolloutStats{})
	if err != nil {
		t.Fatalf("AdvanceGate: %v", err)
	}
	if state.CurrentGate != GateCanaryRollout+1 {
		t.Errorf("expected gate to advance past Gate2 when meta_audit passed and fresh, got gate=%d", state.CurrentGate)
	}
	if state.CanaryPercent != 25 {
		t.Errorf("expected canary_percent=25 (next step after 5), got %d", state.CanaryPercent)
	}
}
