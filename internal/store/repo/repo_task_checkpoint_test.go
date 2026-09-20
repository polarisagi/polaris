package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

// 直接用 SSoT DDL 建表：列清单漂移时测试随之暴露，而不是对着手写 DDL 自证。
func newCheckpointTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("035_task_checkpoints.sql")
	if err != nil {
		t.Fatalf("read ddl: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply ddl: %v", err)
	}
	return db
}

// GR-1.1-001：reason 必须在所有读写路径上往返，否则 handoff 重启恢复会跳过挂起点。
func TestTaskCheckpoint_ReasonRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteTaskCheckpointRepository(newCheckpointTestDB(t))
	row := types.TaskCheckpointRow{
		TaskID: "t1", NodeID: "n1", Attempt: 1, Status: "await_agent",
		TaintLevel: 0, StartedAt: 1, Reason: "handoff_wait", ResumeCtxJSON: `{"v":1}`,
	}
	if err := r.UpsertCheckpoint(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := r.GetCheckpoint(ctx, "t1", "n1", 1)
	if err != nil || got == nil || got.Reason != "handoff_wait" {
		t.Fatalf("GetCheckpoint reason = %+v, err=%v", got, err)
	}
	latest, err := r.GetLatestCheckpoint(ctx, "t1", "n1")
	if err != nil || latest == nil || latest.Reason != "handoff_wait" {
		t.Fatalf("GetLatestCheckpoint reason = %+v, err=%v", latest, err)
	}
	list, err := r.ListCheckpointsByTask(ctx, "t1")
	if err != nil || len(list) != 1 || list[0].Reason != "handoff_wait" || list[0].ResumeCtxJSON != `{"v":1}` {
		t.Fatalf("ListCheckpointsByTask = %+v, err=%v", list, err)
	}
	byStatus, err := r.ListByStatus(ctx, "await_agent")
	if err != nil || len(byStatus) != 1 || byStatus[0].Reason != "handoff_wait" {
		t.Fatalf("ListByStatus = %+v, err=%v", byStatus, err)
	}

	// 冲突更新同样要刷新 reason（恢复后改写为普通状态时清空）。
	row.Status, row.Reason = "done", ""
	if err := r.UpsertCheckpoint(ctx, row); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	got, _ = r.GetCheckpoint(ctx, "t1", "n1", 1)
	if got.Reason != "" {
		t.Fatalf("reason not cleared on conflict update: %q", got.Reason)
	}
}
