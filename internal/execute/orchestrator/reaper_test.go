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

// setupTestDB 建表 DDL 已收敛至 schema_test_helper_test.go 的
// newSchemaBackedDB（直接执行 SSoT .sql 文件），不再手工内联复制。
func setupTestDB(t *testing.T) *sql.DB {
	return newSchemaBackedDB(t)
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
