package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func newPermissionTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"013_chat.sql", "016_preferences.sql", "052_approval_rules.sql"} {
		ddl, rerr := schema.FS.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

// 权限模式：旧键 computer_use_mode 迁移到 permission_mode；未设置取默认；非法值取最严档。
func TestGetPermissionMode_MigrationAndFallbacks(t *testing.T) {
	ctx := context.Background()
	r := NewSQLiteSystemRepository(newPermissionTestDB(t))

	if m, err := r.GetPermissionMode(ctx); err != nil || m != types.ModeAutoReview {
		t.Fatalf("unset -> auto_review, got %q %v", m, err)
	}
	if err := r.UpsertPreference(ctx, types.LegacyPrefComputerUseMode, "full_access"); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.GetPermissionMode(ctx); m != types.ModeFullAccess {
		t.Fatalf("legacy value must migrate, got %q", m)
	}
	if v, _ := r.GetPreference(ctx, types.PrefPermissionMode); v != "full_access" {
		t.Fatalf("migration must write the new key back, got %q", v)
	}
	// 迁移后以新键为准，旧键再变不影响。
	if err := r.UpsertPreference(ctx, types.LegacyPrefComputerUseMode, "default"); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.GetPermissionMode(ctx); m != types.ModeFullAccess {
		t.Fatalf("new key wins after migration, got %q", m)
	}
	if err := r.UpsertPreference(ctx, types.PrefPermissionMode, "root"); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.GetPermissionMode(ctx); m != types.ModeDefault {
		t.Fatalf("invalid stored value must fail closed to default, got %q", m)
	}
	if err := r.SetPermissionMode(ctx, types.PermissionMode("root")); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("SetPermissionMode must validate, got %v", err)
	}
}

func TestApprovalRuleRepo_CRUDIdempotentAndCascade(t *testing.T) {
	ctx := context.Background()
	db := newPermissionTestDB(t)
	rules := NewSQLiteApprovalRuleRepository(db)
	projects := NewSQLiteProjectRepository(db)
	if err := projects.CreateProject(ctx, types.ProjectRow{ID: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}

	a, err := rules.AddRule(ctx, "p1", "cmd", "go test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := rules.AddRule(ctx, "p1", "cmd", "go test")
	if err != nil || a.ID != b.ID {
		t.Fatalf("AddRule must be idempotent: %v %v %v", a, b, err)
	}
	if _, err := rules.AddRule(ctx, "default", "domain", "hnrss.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := rules.AddRule(ctx, "p1", "bogus", "x"); !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("invalid kind must be rejected, got %v", err)
	}
	if _, err := rules.AddRule(ctx, "nope", "cmd", "x"); err == nil {
		t.Fatal("rule for a missing project must be rejected by the foreign key")
	}

	if err := rules.TouchRule(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	list, _ := rules.ListRules(ctx, "p1")
	if len(list) != 1 || list[0].HitCount != 1 || list[0].LastUsedAt == "" {
		t.Fatalf("hit count / last_used_at not recorded: %+v", list)
	}
	if all, _ := rules.ListRules(ctx, ""); len(all) != 2 {
		t.Fatalf("empty project lists everything, got %d", len(all))
	}
	counts, _ := rules.CountByProject(ctx)
	if counts["p1"] != 1 || counts["default"] != 1 {
		t.Fatalf("counts: %v", counts)
	}

	if err := projects.DeleteProject(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := rules.ListRules(ctx, "p1"); len(list) != 0 {
		t.Fatalf("deleting a project must cascade its rules, got %+v", list)
	}
	if err := rules.DeleteRule(ctx, "missing"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("delete missing -> not found, got %v", err)
	}
}
