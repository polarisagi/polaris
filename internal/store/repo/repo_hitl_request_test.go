package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestHITLRequestRepository(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("044_hitl_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	r := NewSQLiteHITLRequestRepository(db)

	if _, err := r.Get(ctx, "x"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}
	base := repo.HITLRequestRow{ID: "a", CheckpointType: "t", PromptJSON: "{}", Status: repo.HITLStatusPending, CreatedAtMs: 1}
	if err := r.Insert(ctx, base); err != nil {
		t.Fatal(err)
	}
	// 同 id 复发覆盖（固定 ID 的调用方）。
	base.PromptJSON = `{"v":2}`
	if err := r.Insert(ctx, base); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, "a"); got.PromptJSON != `{"v":2}` || got.DecidedBy != "" || got.DecidedAtMs != 0 {
		t.Fatalf("upsert: %+v", got)
	}
	if hit, err := r.Decide(ctx, "a", repo.HITLStatusApproved, repo.HITLByHuman, "ok", `{}`); err != nil || !hit {
		t.Fatalf("first decide: %v %v", hit, err)
	}
	if hit, err := r.Decide(ctx, "a", repo.HITLStatusDenied, repo.HITLByHuman, "", ""); err != nil || hit {
		t.Fatalf("second decide must miss: %v %v", hit, err)
	}
	if hit, _ := r.Decide(ctx, "nope", repo.HITLStatusDenied, repo.HITLByHuman, "", ""); hit {
		t.Fatal("unknown id must miss")
	}
	if p, _ := r.ListPending(ctx); len(p) != 0 {
		t.Fatalf("pending: %+v", p)
	}
	// 对已决行的 Decide 是无副作用空操作（WHERE status='pending' 不命中，不触发 CHECK）。
	if _, err := r.Decide(ctx, "a", repo.HITLStatusDenied, "bogus", "", ""); err != nil {
		t.Fatalf("decide on non-pending row should be a no-op, got %v", err)
	}
	// 非法 decided_by 被 CHECK 拒绝。
	bad := base
	bad.ID, bad.Status, bad.DecidedBy = "b", repo.HITLStatusApproved, "bogus"
	if err := r.Insert(ctx, bad); err == nil {
		t.Fatal("expected CHECK violation")
	}
}
