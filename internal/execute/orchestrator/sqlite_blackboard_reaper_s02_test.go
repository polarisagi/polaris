package orchestrator

import (
	"context"
	"testing"
	"time"
)

// TestReaperPhase2_ZombieBroadcast_OnlyAfterCommit_S02 验证阶段02修复：
// reaperPhase2 必须先 tx.Commit() 成功，再 broadcast task_failed 事件。回归锚点：
// 修复前 broadcast 发生在 Commit 之前，若 Commit 失败会出现"订阅者已收到
// task_failed 但 DB 里该任务其实还是 running"的不一致。本测试固定 happy path
// 下的执行顺序不变量：broadcast 到达时，DB 里对应任务必须已经是 'failed'。
func TestReaperPhase2_ZombieBroadcast_OnlyAfterCommit_S02(t *testing.T) {
	db := setupTestDB(t)
	bb := NewSQLiteBlackboard(db)
	ctx := context.Background()

	taskID := "zombie_task_1"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (task_id, session_id, status, claimed_by, version, created_at, updated_at)
		VALUES (?, 'session', 'running', 'agent1', 1, datetime('now'), datetime('now', '-40 minute'))`, taskID); err != nil {
		t.Fatalf("insert zombie task: %v", err)
	}

	events, err := bb.Subscribe(ctx)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	bb.reaperPhase2(ctx)

	select {
	case ev := <-events:
		if ev.Type != "task_failed" || ev.TaskID != taskID {
			t.Fatalf("unexpected event: %+v", ev)
		}
		// broadcast 已到达：此刻 DB 状态必须已经是 'failed'（commit 先于 broadcast）。
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE task_id=?`, taskID).Scan(&status); err != nil {
			t.Fatalf("query status: %v", err)
		}
		if status != "failed" {
			t.Errorf("broadcast arrived before commit was durable: status=%q, want 'failed'", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task_failed broadcast")
	}
}
