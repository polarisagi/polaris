package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
)

// newTestModelVersionDB 创建内存 SQLite 并建表，DDL 与
// internal/protocol/schema/033_model_version_registry.sql 保持一致。
func newTestModelVersionDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ddl := `
	CREATE TABLE IF NOT EXISTS model_version_entries (
		id                  TEXT PRIMARY KEY,
		provider            TEXT NOT NULL,
		model_id            TEXT NOT NULL,
		version             TEXT NOT NULL DEFAULT '',
		deprecated          INTEGER NOT NULL DEFAULT 0,
		successor_model_id  TEXT NOT NULL DEFAULT '',
		prompt_template     TEXT NOT NULL DEFAULT '',
		tool_call_style     TEXT NOT NULL DEFAULT '',
		max_context         INTEGER NOT NULL DEFAULT 0,
		capabilities        TEXT NOT NULL DEFAULT '{}',
		validated_on        TEXT NOT NULL DEFAULT '[]',
		compatibility_score REAL NOT NULL DEFAULT 1.0,
		consecutive_errors  INTEGER NOT NULL DEFAULT 0,
		updated_at          INTEGER NOT NULL DEFAULT 0
	);`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestSQLiteModelVersionRepository_UpsertAndGet(t *testing.T) {
	db := newTestModelVersionDB(t)
	r := NewSQLiteModelVersionRepository(db)
	ctx := context.Background()

	entry := &protorepo.ModelVersionEntry{
		ID: "anthropic:claude-3-opus-20240229", Provider: "anthropic", ModelID: "claude-3-opus-20240229",
		Deprecated: true, SuccessorModelID: "claude-3-5-sonnet-latest",
		CompatibilityScore: 0.95, ValidatedOn: `["skill-a"]`, Capabilities: `{"tool_call":true}`,
		UpdatedAt: 1000,
	}
	if err := r.Upsert(ctx, entry); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := r.Get(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected entry, got nil")
	}
	if !got.Deprecated || got.SuccessorModelID != "claude-3-5-sonnet-latest" || got.CompatibilityScore != 0.95 {
		t.Errorf("unexpected entry: %+v", got)
	}

	// Upsert 覆盖更新
	entry.CompatibilityScore = 0.5
	if err := r.Upsert(ctx, entry); err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}
	got2, err := r.Get(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got2.CompatibilityScore != 0.5 {
		t.Errorf("expected updated score 0.5, got %v", got2.CompatibilityScore)
	}
}

func TestSQLiteModelVersionRepository_GetMissingReturnsNilNoError(t *testing.T) {
	db := newTestModelVersionDB(t)
	r := NewSQLiteModelVersionRepository(db)

	got, err := r.Get(context.Background(), "nope:nope")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil entry, got %+v", got)
	}
}

func TestSQLiteModelVersionRepository_ListDeprecated(t *testing.T) {
	db := newTestModelVersionDB(t)
	r := NewSQLiteModelVersionRepository(db)
	ctx := context.Background()

	must := func(e *protorepo.ModelVersionEntry) {
		if err := r.Upsert(ctx, e); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	must(&protorepo.ModelVersionEntry{ID: "p:a", Provider: "p", ModelID: "a", Deprecated: true, CompatibilityScore: 1})
	must(&protorepo.ModelVersionEntry{ID: "p:b", Provider: "p", ModelID: "b", Deprecated: false, CompatibilityScore: 1})

	deprecated, err := r.ListDeprecated(ctx)
	if err != nil {
		t.Fatalf("ListDeprecated: %v", err)
	}
	if len(deprecated) != 1 || deprecated[0].ID != "p:a" {
		t.Errorf("expected only p:a, got %+v", deprecated)
	}

	all, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 entries, got %d", len(all))
	}
}

func TestSQLiteModelVersionRepository_FindPredecessor(t *testing.T) {
	db := newTestModelVersionDB(t)
	r := NewSQLiteModelVersionRepository(db)
	ctx := context.Background()

	if err := r.Upsert(ctx, &protorepo.ModelVersionEntry{
		ID: "openai:gpt-4", Provider: "openai", ModelID: "gpt-4",
		Deprecated: true, SuccessorModelID: "gpt-4o-mini", CompatibilityScore: 1,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	pred, err := r.FindPredecessor(ctx, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("FindPredecessor: %v", err)
	}
	if pred == nil || pred.ModelID != "gpt-4" {
		t.Errorf("expected predecessor gpt-4, got %+v", pred)
	}

	none, err := r.FindPredecessor(ctx, "openai", "no-such-successor")
	if err != nil {
		t.Fatalf("FindPredecessor (none): %v", err)
	}
	if none != nil {
		t.Errorf("expected nil, got %+v", none)
	}
}

func TestSQLiteModelVersionRepository_Delete(t *testing.T) {
	db := newTestModelVersionDB(t)
	r := NewSQLiteModelVersionRepository(db)
	ctx := context.Background()

	if err := r.Upsert(ctx, &protorepo.ModelVersionEntry{ID: "p:a", Provider: "p", ModelID: "a", CompatibilityScore: 1}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Delete(ctx, "p:a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := r.Get(ctx, "p:a")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after delete, got %+v", got)
	}
}
