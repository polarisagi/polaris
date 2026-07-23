package orchestrator

import (
	"context"
	"database/sql"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"
)

func newMockSQLiteDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE tasks (
			task_id TEXT PRIMARY KEY,
			session_id TEXT,
			status TEXT,
			priority INTEGER,
			claimed_by TEXT,
			claimed_at DATETIME,
			expires_at DATETIME,
			version INTEGER,
			tokens_input INTEGER,
			tokens_output INTEGER,
			tokens_cache_read INTEGER,
			cost_usd REAL,
			retry_count INTEGER DEFAULT 0,
			max_retries INTEGER DEFAULT 3,
			provider_suspended_count INTEGER DEFAULT 0,
			error TEXT,
			namespace TEXT,
			intent BLOB,
			trace_id TEXT,
			span_id TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE events (
			id TEXT PRIMARY KEY,
			topic TEXT,
			actor TEXT,
			type TEXT,
			payload TEXT,
			created_at INTEGER
		);
	`)
	return db, err
}

func TestSQLiteBlackboard(t *testing.T) {
	db, err := newMockSQLiteDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	bb := NewSQLiteBlackboard(db)
	ctx := context.Background()

	// Setup registry
	registry := NewAgentRegistry()
	registry.Register("agent-1", AgentCard{Name: "agent-1", MaxDepth: 5}, nil)
	bb.SetRegistry(registry)

	// MaxDepth
	if bb.resolveMaxDepth("agent-1") != 5 {
		t.Errorf("expected max depth 5")
	}
	if bb.resolveMaxDepth("unknown") != MaxSpawnDepth {
		t.Errorf("expected default max depth")
	}

	// PostTask
	task1 := &types.TaskEntry{
		ID:         "task-1",
		Type:       "agent-1",
		SpawnDepth: 1,
	}
	err = bb.PostTask(ctx, task1)
	if err != nil {
		t.Fatal(err)
	}

	// ClaimTask
	_, err = bb.ClaimTask(ctx, "task-1", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// CompleteTask
	err = bb.CompleteTask(ctx, "task-1", "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Register cancel func
	var canceled bool
	bb.RegisterCancelFunc("task-2", func() { canceled = true })
	bb.removeCancelFunc("task-2")
	if canceled {
		t.Errorf("should not be canceled")
	}

	// Add another task for lifecycle testing
	task3 := &types.TaskEntry{
		ID:         "task-3",
		Type:       "agent-1",
		SpawnDepth: 1,
	}
	_ = bb.PostTask(ctx, task3)
	_, _ = bb.ClaimTask(ctx, "task-3", "worker-1")

	// Test PeekTask
	snap, err := bb.PeekTask(ctx, "task-3")
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil || snap.ID != "task-3" {
		t.Errorf("expected task-3 snapshot")
	}

	// Test StartExecution
	err = bb.StartExecution(ctx, "task-3", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Test SuspendForHITL
	err = bb.SuspendForHITL(ctx, "task-3", "worker-1", 100)
	if err != nil {
		t.Fatal(err)
	}

	// Test ResumeFromHITL
	err = bb.ResumeFromHITL(ctx, "task-3", "worker-1", true)
	if err != nil {
		t.Fatal(err)
	}

	// Test BeginCompensation and EndCompensation
	err = bb.BeginCompensation(ctx, "task-3", "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	err = bb.EndCompensation(ctx, "task-3", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Test UpdateTaskTokens
	err = bb.UpdateTaskTokens(ctx, "task-3", 100, 50, 0, 0.01)
	if err != nil {
		t.Fatal(err)
	}

	// Test PostBatch
	taskBatch := []*types.TaskEntry{
		{ID: "batch-1", Type: "agent-1"},
		{ID: "batch-2", Type: "agent-1"},
	}
	err = bb.PostBatch(ctx, taskBatch)
	if err != nil {
		t.Fatal(err)
	}

	// Test FailTask
	_, _ = bb.ClaimTask(ctx, "batch-1", "worker-1")
	err = bb.FailTask(ctx, "batch-1", "worker-1", []byte("failed for some reason"))
	if err != nil {
		t.Fatal(err)
	}

	// Test RenewLease
	_, _ = bb.ClaimTask(ctx, "batch-2", "worker-1")
	err = bb.RenewLease(ctx, "batch-2", "worker-1")
	if err != nil {
		t.Fatal(err)
	}

	// Test SideEffectPreCheck
	_ = bb.StartExecution(ctx, "batch-2", "worker-1")
	err = bb.SideEffectPreCheck(ctx, "batch-2", "worker-1", 3)
	if err != nil {
		t.Fatal(err)
	}

	// Test StopAll
	_ = bb.StopAll(ctx, "stop requested")

	// Test ResumeFromSuspended
	_ = bb.ResumeFromSuspended(ctx, "task-3")

	// Test Reap
	task2 := &types.TaskEntry{
		ID:         "task-2",
		Type:       "agent-1",
		SpawnDepth: 1,
	}
	_ = bb.PostTask(ctx, task2)
	_, _ = bb.ClaimTask(ctx, "task-2", "worker-1")
	// To test expiration, we have to manually update expires_at
	_, _ = db.Exec(`UPDATE tasks SET expires_at = datetime('now', '-1 hour') WHERE task_id = 'task-2'`)

	bb.reap(ctx)
	// count check depends on if reap actually modifies or just cleans.
	var status string
	_ = db.QueryRow(`SELECT status FROM tasks WHERE task_id='task-2'`).Scan(&status)
	if status != "pending" {
		t.Errorf("expected task-2 to be reverted to pending, got %s", status)
	}
}

// TestSQLiteBlackboard_NamespacePersistence 验证 GD-14-001：TaskEntry.Namespace
// 经 PostTask/PostBatch 落盘后，PeekTask 能正确透传回 TaskSnapshot.Namespace，
// 供 Worker 读取后注入 AgentKernel.SetMemoryNamespace。
func TestSQLiteBlackboard_NamespacePersistence(t *testing.T) {
	db, err := newMockSQLiteDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	bb := NewSQLiteBlackboard(db)
	ctx := context.Background()

	// PostTask 单条：带命名空间
	nsTask := &types.TaskEntry{ID: "ns-task-1", Type: "worker", Namespace: "swarm-ns-A"}
	if err := bb.PostTask(ctx, nsTask); err != nil {
		t.Fatalf("PostTask failed: %v", err)
	}
	snap, err := bb.PeekTask(ctx, "ns-task-1")
	if err != nil {
		t.Fatalf("PeekTask failed: %v", err)
	}
	if snap == nil || snap.Namespace != "swarm-ns-A" {
		t.Fatalf("expected Namespace=swarm-ns-A, got %+v", snap)
	}

	// PostTask：未设置 Namespace 的任务，PeekTask 应返回空字符串（默认行为不变）。
	plainTask := &types.TaskEntry{ID: "plain-task-1", Type: "worker"}
	if err := bb.PostTask(ctx, plainTask); err != nil {
		t.Fatalf("PostTask (plain) failed: %v", err)
	}
	plainSnap, err := bb.PeekTask(ctx, "plain-task-1")
	if err != nil {
		t.Fatalf("PeekTask (plain) failed: %v", err)
	}
	if plainSnap == nil || plainSnap.Namespace != "" {
		t.Fatalf("expected empty Namespace for plain task, got %+v", plainSnap)
	}

	// PostBatch：批量任务共享同一命名空间（CSV fanout 场景）。
	batch := []*types.TaskEntry{
		{ID: "ns-batch-1", Type: "worker", Namespace: "swarm-ns-B"},
		{ID: "ns-batch-2", Type: "worker", Namespace: "swarm-ns-B"},
	}
	if err := bb.PostBatch(ctx, batch); err != nil {
		t.Fatalf("PostBatch failed: %v", err)
	}
	for _, id := range []string{"ns-batch-1", "ns-batch-2"} {
		s, err := bb.PeekTask(ctx, id)
		if err != nil {
			t.Fatalf("PeekTask(%s) failed: %v", id, err)
		}
		if s == nil || s.Namespace != "swarm-ns-B" {
			t.Errorf("expected %s Namespace=swarm-ns-B, got %+v", id, s)
		}
	}
}
