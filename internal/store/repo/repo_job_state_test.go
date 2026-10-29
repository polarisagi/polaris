package repo

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

// 直接用 SSoT DDL 建表，列清单漂移时测试随之暴露。
func newJobStateTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("041_background_job_state.sql")
	if err != nil {
		t.Fatalf("read ddl: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply ddl: %v", err)
	}
	return db
}

func TestBackgroundJobState_RoundTripAndUpsert(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteBackgroundJobStateRepository(newJobStateTestDB(t))

	if _, found, err := r.GetLastRun(ctx, "idle_forgetting"); err != nil || found {
		t.Fatalf("空表应 found=false, err=nil；got found=%v err=%v", found, err)
	}

	first := time.UnixMilli(time.Now().Add(-2 * time.Hour).UnixMilli())
	if err := r.RecordRun(ctx, "idle_forgetting", "no_work", first); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, found, err := r.GetLastRun(ctx, "idle_forgetting")
	if err != nil || !found || !got.Equal(first) {
		t.Fatalf("往返不一致: got=%v found=%v err=%v want=%v", got, found, err, first)
	}

	second := first.Add(time.Hour)
	if err := r.RecordRun(ctx, "idle_forgetting", "success", second); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, _ = r.GetLastRun(ctx, "idle_forgetting")
	if !got.Equal(second) {
		t.Fatalf("upsert 应覆盖旧值: got=%v want=%v", got, second)
	}

	if err := r.RecordRun(ctx, "idle_graph_prune", "bogus", second); err == nil {
		t.Fatal("非法 status 应被 CHECK 约束拒绝")
	}
}
