package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func postAndClaim(t *testing.T, bb *SQLiteBlackboard, taskID, agentID string) {
	t.Helper()
	ctx := context.Background()
	if err := bb.PostTask(ctx, &types.TaskEntry{ID: taskID, Type: "t"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := bb.ClaimTask(ctx, taskID, agentID); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
}

// GR-6.2-001：running 态任务必须可续约。
func TestRenewLease_RunningTask(t *testing.T) {
	bb := NewSQLiteBlackboard(newSchemaBackedDB(t))
	postAndClaim(t, bb, "r1", "w1")
	if err := bb.StartExecution(context.Background(), "r1", "w1"); err != nil {
		t.Fatal(err)
	}
	if err := bb.RenewLease(context.Background(), "r1", "w1"); err != nil {
		t.Fatalf("renew running task: %v", err)
	}
}

// GR-6.2-003：别的 agent 对已 running 的任务调用 StartExecution 不得得到成功。
func TestStartExecution_IdempotentRequiresOwner(t *testing.T) {
	bb := NewSQLiteBlackboard(newSchemaBackedDB(t))
	postAndClaim(t, bb, "s1", "owner")
	ctx := context.Background()
	if err := bb.StartExecution(ctx, "s1", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := bb.StartExecution(ctx, "s1", "owner"); err != nil {
		t.Fatalf("owner idempotent retry should succeed: %v", err)
	}
	if err := bb.StartExecution(ctx, "s1", "intruder"); err == nil {
		t.Fatal("non-owner StartExecution must fail")
	}
}

// 隐藏缺陷：expires_at(RFC3339 'T') 与 datetime('now')(' ') 字符串比较，
// 同一 UTC 日内过期的租约永远扫不出来。
func TestReap_SameDayExpiredLease(t *testing.T) {
	db := newSchemaBackedDB(t)
	bb := NewSQLiteBlackboard(db)
	postAndClaim(t, bb, "e1", "w1")
	past := time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE tasks SET expires_at=? WHERE task_id='e1'`, past); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status IN ('claimed','running') AND datetime(expires_at) < datetime('now')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired lease not detected, n=%d", n)
	}
}

// GR-6.2-005：HoldLease 必须向黑板登记取消函数，CancelTask/Reaper 可据此中止执行。
func TestHoldLease_RegistersCancel(t *testing.T) {
	bb := NewSQLiteBlackboard(newSchemaBackedDB(t))
	postAndClaim(t, bb, "h1", "w1")
	ctx, release := HoldLease(context.Background(), bb, "h1", "w1")
	defer release()
	bb.mu.Lock()
	cancel := bb.cancels["h1"]
	bb.mu.Unlock()
	if cancel == nil {
		t.Fatal("cancel func not registered")
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("exec ctx not cancelled by registered cancel func")
	}
}

// 哨兵错误码不得与 CodeInternal 相同，否则任意 DB 故障都会被 errors.Is 判为租约失效。
func TestLeaseSentinels_DistinctCodes(t *testing.T) {
	dbErr := apperr.Wrap(apperr.CodeInternal, "db down", errors.New("io"))
	if errors.Is(dbErr, ErrStaleBlackboardLease) || errors.Is(dbErr, ErrTaskNotOwned) {
		t.Fatal("internal DB error must not match lease sentinels")
	}
	if errors.Is(ErrStaleBlackboardLease, ErrTaskNotOwned) {
		t.Fatal("sentinels must be distinguishable")
	}
}
