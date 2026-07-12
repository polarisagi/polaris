package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	_ "github.com/mattn/go-sqlite3"
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
			error TEXT,
			version INTEGER,
			retry_count INTEGER DEFAULT 0,
			max_retries INTEGER DEFAULT 3,
			namespace TEXT,
			intent BLOB,
			created_at DATETIME,
			updated_at DATETIME
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
	return db
}

func TestReaperCancelGracePeriod(t *testing.T) {
	db := setupTestDB(t)
	bb := NewSQLiteBlackboard(db)
	reaper := NewReaper(bb)

	ctx := context.Background()
	task := &types.TaskEntry{ID: "malicious_task"}
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

func TestReaperCancelParallel(t *testing.T) {
	db := setupTestDB(t)
	bb := NewSQLiteBlackboard(db)
	ctx := context.Background()

	// 模拟 100 个需要 cancel 的过期任务
	const numTasks = 100
	var cancelTriggered []chan struct{}
	for i := 0; i < numTasks; i++ {
		taskID := fmt.Sprintf("task_%d", i)
		_, err := bb.db.ExecContext(context.Background(), `INSERT INTO tasks (task_id, session_id, status, claimed_by, expires_at, created_at, updated_at)
			VALUES (?, 'session', ?, 'agent1', datetime('now', '-10 seconds'), datetime('now'), datetime('now'))`, taskID, "claimed")
		if err != nil {
			t.Fatalf("insert failed: %v", err)
		}

		cancelCtx, cancel := context.WithCancel(context.Background())
		bb.RegisterCancelFunc(taskID, cancel)

		ch := make(chan struct{})
		cancelTriggered = append(cancelTriggered, ch)
		go func(c context.Context, ch chan struct{}) {
			<-c.Done()
			// 模拟慢 cancel
			time.Sleep(10 * time.Millisecond)
			close(ch)
		}(cancelCtx, ch)
	}

	start := time.Now()
	// 使用 bb.reap(ctx) 测试
	bb.reap(ctx)
	elapsed := time.Since(start)

	if elapsed > 5500*time.Millisecond {
		t.Errorf("reap took %v, expected parallel cancel taking ~5s", elapsed)
	}

	for _, ch := range cancelTriggered {
		select {
		case <-ch:
		default:
			t.Errorf("cancel func was not called!")
		}
	}
}
