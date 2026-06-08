package swarm

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol"
)

func setupTestDB(t *testing.T) *sql.DB {
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
			priority INTEGER,
			status TEXT,
			claimed_by TEXT,
			claimed_at DATETIME,
			expires_at DATETIME,
			provider_suspended_count INTEGER DEFAULT 0,
			suspend_reason TEXT,
			version INTEGER,
			created_at DATETIME,
			updated_at DATETIME
		)
	`)
	if err != nil {
		t.Fatalf("failed to create tasks table: %v", err)
	}
	return db
}

func TestReaperCancelGracePeriod(t *testing.T) {
	db := setupTestDB(t)
	bb := NewSQLiteBlackboard(db)
	reaper := NewReaper(bb)

	ctx := context.Background()
	task := &protocol.TaskEntry{ID: "malicious_task"}
	bb.PostTask(ctx, task)

	claimed, err := bb.ClaimTask(ctx, task.ID, "bad_agent")
	if err != nil || !claimed {
		t.Fatalf("failed to claim")
	}

	db.Exec(`UPDATE tasks SET expires_at = datetime('now', '-10 seconds') WHERE task_id = 'malicious_task'`)

	cancelCtx, cancel := context.WithCancel(context.Background())
	bb.RegisterCancelFunc(task.ID, cancel)

	cancelTriggered := make(chan struct{})
	go func() {
		<-cancelCtx.Done()
		close(cancelTriggered)
	}()

	start := time.Now()
	reaper.Phase1(ctx)

	elapsed := time.Since(start)
	if elapsed < 5*time.Second {
		t.Errorf("expected graceful shutdown to wait ~5s, but got %v", elapsed)
	}

	select {
	case <-cancelTriggered:
		// success
	default:
		t.Errorf("cancel func was not called!")
	}
}
