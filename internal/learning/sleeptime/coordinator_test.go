package sleeptime_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/learning/harnessoverlay"
	"github.com/polarisagi/polaris/internal/learning/sleeptime"
)

type mockAuditTrail struct {
	records []auditRecord
}

type auditRecord struct {
	actor  string
	action string
	target string
}

func (m *mockAuditTrail) Record(ctx context.Context, actor, action, target string, metadata map[string]any) error {
	m.records = append(m.records, auditRecord{actor: actor, action: action, target: target})
	return nil
}

type mockProposer struct {
	proposals []sleeptime.ProposedChange
	err       error
}

func (m *mockProposer) Propose(ctx context.Context, weaknesses []sleeptime.Weakness) ([]sleeptime.ProposedChange, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.proposals, nil
}

type mockOffPeakGate struct {
	allow bool
}

func (m *mockOffPeakGate) Allow() bool {
	return m.allow
}

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	schema := `
	CREATE TABLE IF NOT EXISTS llm_calls (
		id               TEXT PRIMARY KEY,
		created_at       INTEGER NOT NULL,
		session_id       TEXT    NOT NULL DEFAULT '',
		purpose          TEXT    NOT NULL DEFAULT '',
		provider         TEXT    NOT NULL DEFAULT '',
		status           TEXT    NOT NULL,
		error            TEXT    NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS chat_sessions (
		id               TEXT PRIMARY KEY,
		title            TEXT NOT NULL DEFAULT '',
		updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create tables: %v", err)
	}
	return db
}

func TestCoordinator_RunClosedLoop_AcceptAndReject(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	defer db.Close()

	// 插入失败记录
	_, err := db.Exec(`
		INSERT INTO llm_calls (id, created_at, session_id, purpose, status, error)
		VALUES ('1', 1000, 's1', 'agentic_loop', 'error', 'command timeout')
	`)
	if err != nil {
		t.Fatalf("insert llm_calls failed: %v", err)
	}

	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "harness_overlay")
	overlayMgr := harnessoverlay.New(overlayDir)
	if err := overlayMgr.Init(ctx); err != nil {
		t.Fatalf("overlay init failed: %v", err)
	}

	proposer := &mockProposer{
		proposals: []sleeptime.ProposedChange{
			{Path: "tool_notes/bash.md", Content: "Avoid sleeps.", Reason: "Fix timeout"},
		},
	}
	audit := &mockAuditTrail{}

	// Case 1: 评估通过 -> 接受并提交
	evalAccept := &mockEvaluator{baseIn: 0.70, baseOut: 0.75, candIn: 0.80, candOut: 0.76}
	coord := sleeptime.NewCoordinator(sleeptime.CoordinatorConfig{
		OverlayMgr: overlayMgr,
		Evaluator:  evalAccept,
		Proposer:   proposer,
		DB:         db,
		AuditTrail: audit,
		Enabled:    true,
	})

	res, err := coord.RunClosedLoop(ctx)
	if err != nil {
		t.Fatalf("RunClosedLoop failed: %v", err)
	}
	if !res.Accepted {
		t.Errorf("expected proposal to be accepted, got false (reason: %s)", res.RejectionReason)
	}
	if res.CommitHash == "" {
		t.Errorf("expected commit hash on acceptance")
	}
	if len(audit.records) == 0 || audit.records[len(audit.records)-1].action != "harness_overlay_commit" {
		t.Errorf("expected audit action 'harness_overlay_commit', got %+v", audit.records)
	}

	// 验证覆盖层已被写入
	note, err := overlayMgr.GetToolNote(ctx, "bash")
	if err != nil || note != "Avoid sleeps." {
		t.Errorf("unexpected tool note after accept: %q, err: %v", note, err)
	}

	// Case 2: 评估退化 -> 拒绝，不提交
	evalReject := &mockEvaluator{baseIn: 0.70, baseOut: 0.75, candIn: 0.85, candOut: 0.65}
	coordReject := sleeptime.NewCoordinator(sleeptime.CoordinatorConfig{
		OverlayMgr: overlayMgr,
		Evaluator:  evalReject,
		Proposer:   proposer,
		DB:         db,
		AuditTrail: audit,
		Enabled:    true,
	})

	resReject, err := coordReject.RunClosedLoop(ctx)
	if err != nil {
		t.Fatalf("RunClosedLoop failed: %v", err)
	}
	if resReject.Accepted {
		t.Errorf("expected proposal to be rejected, got accepted")
	}
	if resReject.CommitHash != "" {
		t.Errorf("expected empty commit hash on rejection, got %s", resReject.CommitHash)
	}
	if audit.records[len(audit.records)-1].action != "harness_overlay_rejected" {
		t.Errorf("expected audit action 'harness_overlay_rejected', got %+v", audit.records)
	}
}

func TestCoordinator_RunSleepTime_GatingAndConsolidation(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	defer db.Close()

	// 插入一个旧会话（20 分钟前更新）
	oldTime := time.Now().UTC().Add(-20 * time.Minute).Format("2006-01-02T15:04:05Z")
	_, err := db.Exec(`INSERT INTO chat_sessions (id, title, updated_at) VALUES ('idle_sess_1', 'Old Session', ?)`, oldTime)
	if err != nil {
		t.Fatalf("insert chat_sessions failed: %v", err)
	}

	tmpDir := t.TempDir()
	overlayMgr := harnessoverlay.New(tmpDir)
	_ = overlayMgr.Init(ctx)

	consolidatedSessions := make([]string, 0)
	consolidateFn := func(ctx context.Context, sessionID string) error {
		consolidatedSessions = append(consolidatedSessions, sessionID)
		return nil
	}

	offPeak := &mockOffPeakGate{allow: false}
	eval := &mockEvaluator{baseIn: 0.8, baseOut: 0.8, candIn: 0.8, candOut: 0.8}
	proposer := &mockProposer{}

	coord := sleeptime.NewCoordinator(sleeptime.CoordinatorConfig{
		OverlayMgr:  overlayMgr,
		Evaluator:   eval,
		Proposer:    proposer,
		DB:          db,
		OffPeak:     offPeak,
		Consolidate: consolidateFn,
		Enabled:     true,
		IdleMinutes: 10,
	})

	// 1. 错峰窗口未允许 -> 跳过
	if err := coord.RunSleepTime(ctx); err != nil {
		t.Fatalf("RunSleepTime failed: %v", err)
	}
	if len(consolidatedSessions) != 0 {
		t.Errorf("expected 0 consolidated sessions when off-peak disabled, got %d", len(consolidatedSessions))
	}

	// 2. 错峰窗口允许 -> 触发闲置会话整理
	offPeak.allow = true
	if err := coord.RunSleepTime(ctx); err != nil {
		t.Fatalf("RunSleepTime failed: %v", err)
	}
	if len(consolidatedSessions) != 1 || consolidatedSessions[0] != "idle_sess_1" {
		t.Errorf("expected ['idle_sess_1'] consolidated, got %+v", consolidatedSessions)
	}

	// 3. Coordinator disabled -> 跳过
	coord.SetEnabled(false)
	consolidatedSessions = nil
	if err := coord.RunSleepTime(ctx); err != nil {
		t.Fatalf("RunSleepTime failed: %v", err)
	}
	if len(consolidatedSessions) != 0 {
		t.Errorf("expected 0 consolidated sessions when coordinator disabled, got %d", len(consolidatedSessions))
	}
}
