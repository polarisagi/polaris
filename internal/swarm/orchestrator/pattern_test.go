package orchestrator

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"
)

func setupPatternBlackboard(t *testing.T) *SQLiteBlackboard {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	// in-memory SQLite 每个连接是独立的数据库，必须限制为单连接
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE tasks (
			task_id TEXT PRIMARY KEY,
			session_id TEXT,
			type TEXT,
			priority INTEGER DEFAULT 0,
			status TEXT DEFAULT 'pending',
			claimed_by TEXT,
			claimed_at DATETIME,
			expires_at DATETIME,
			provider_suspended_count INTEGER DEFAULT 0,
			suspend_reason TEXT,
			error TEXT,
			version INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT (datetime('now')),
			updated_at DATETIME DEFAULT (datetime('now'))
		)
	`)
	if err != nil {
		t.Fatalf("failed to create tasks table: %v", err)
	}

	// writeTaskEvent 需要 events 表（inv_M8_02 事务内双写）
	_, err = db.Exec(`
		CREATE TABLE events (
			offset    INTEGER PRIMARY KEY AUTOINCREMENT,
			id        TEXT NOT NULL UNIQUE,
			topic     TEXT NOT NULL,
			actor     TEXT NOT NULL,
			type      TEXT NOT NULL,
			payload   BLOB NOT NULL,
			created_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("failed to create events table: %v", err)
	}

	return NewSQLiteBlackboard(db)
}

func mockPatternWorker(ctx context.Context, bb *SQLiteBlackboard, taskID, agentID string, delay time.Duration, result []byte) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			claimed, err := bb.ClaimTask(ctx, taskID, agentID)
			if err == nil && claimed {
				time.Sleep(delay)
				bb.CompleteTask(ctx, taskID, agentID, result) //nolint:errcheck
				return
			}
		}
	}
}

func TestParallelExecutor(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewParallelExecutor(bb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tasks := []types.TaskEntry{
		{ID: "t1"},
		{ID: "t2"},
	}

	go func() { mockPatternWorker(ctx, bb, "t1", "agent1", 50*time.Millisecond, []byte("res1")) }()
	go func() { mockPatternWorker(ctx, bb, "t2", "agent2", 50*time.Millisecond, []byte("res2")) }()

	if err := executor.Execute(ctx, "parent", tasks); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMapReduceExecutor(t *testing.T) {
	bb := setupPatternBlackboard(t)
	executor := NewMapReduceExecutor(bb, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tasks := []types.TaskEntry{
		{ID: "m1"},
		{ID: "m2"},
	}

	go func() { mockPatternWorker(ctx, bb, "m1", "agent1", 50*time.Millisecond, []byte("A")) }()
	go func() { mockPatternWorker(ctx, bb, "m2", "agent2", 50*time.Millisecond, []byte("B")) }()

	res, err := executor.Execute(ctx, "parent", tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected non-empty reduced results")
	}
}
