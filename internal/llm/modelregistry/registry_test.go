package modelregistry

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
)

func newTestRegistry(t *testing.T) (*Registry, *sql.DB) {
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
	repoImpl := storerepo.NewSQLiteModelVersionRepository(db)
	return NewRegistry(repoImpl), db
}

func TestDecideMigration_ThreeTiers(t *testing.T) {
	cases := []struct {
		score float64
		want  MigrationDecision
	}{
		{1.0, MigrationAuto},
		{0.9, MigrationAuto},
		{0.89, MigrationAutoWithWarn},
		{0.7, MigrationAutoWithWarn},
		{0.69, MigrationManualOnly},
		{0.0, MigrationManualOnly},
	}
	for _, c := range cases {
		if got := DecideMigration(c.score); got != c.want {
			t.Errorf("DecideMigration(%v) = %v, want %v", c.score, got, c.want)
		}
	}
}

type fakeTester struct {
	results map[string]bool
}

func (f *fakeTester) TestSkillCompat(_ context.Context, _, _, skillName string) (bool, error) {
	return f.results[skillName], nil
}

func TestOnModelUpgrade_UpdatesCompatibilityScore(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	tester := &fakeTester{results: map[string]bool{"skill-a": true, "skill-b": true, "skill-c": false}}
	if err := reg.OnModelUpgrade(ctx, "anthropic", "claude-4", []string{"skill-a", "skill-b", "skill-c"}, tester); err != nil {
		t.Fatalf("OnModelUpgrade: %v", err)
	}

	entry, err := reg.Get(ctx, "anthropic", "claude-4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry to be created")
	}
	wantScore := 2.0 / 3.0
	if entry.CompatibilityScore != wantScore {
		t.Errorf("expected score %v, got %v", wantScore, entry.CompatibilityScore)
	}
	if entry.ValidatedOn != `["skill-a","skill-b"]` {
		t.Errorf("unexpected ValidatedOn: %v", entry.ValidatedOn)
	}
}

func TestOnModelUpgrade_NilTesterSkipsRetest(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	if err := reg.OnModelUpgrade(ctx, "openai", "gpt-5", []string{"skill-a"}, nil); err != nil {
		t.Fatalf("OnModelUpgrade: %v", err)
	}
	entry, err := reg.Get(ctx, "openai", "gpt-5")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry to be created even without tester")
	}
	if entry.CompatibilityScore != 1.0 {
		t.Errorf("expected default score 1.0 when tester is nil, got %v", entry.CompatibilityScore)
	}
}

func TestDeprecateModel_TriggersReindexOnlyForEmbeddingModels(t *testing.T) {
	reg, db := newTestRegistry(t)
	ctx := context.Background()

	var triggered []string
	reg.reindexTrigger = func(_ context.Context, provider, modelID string) error {
		triggered = append(triggered, provider+":"+modelID)
		return nil
	}

	// 先注册一个 embedding=true 的模型条目
	repoImpl := storerepo.NewSQLiteModelVersionRepository(db)
	if err := repoImpl.Upsert(ctx, &protorepo.ModelVersionEntry{
		ID: "local:bge-small", Provider: "local", ModelID: "bge-small",
		Capabilities: `{"embedding":true}`, CompatibilityScore: 1, ValidatedOn: "[]",
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	if err := reg.DeprecateModel(ctx, "local", "bge-small", "bge-large"); err != nil {
		t.Fatalf("DeprecateModel: %v", err)
	}
	if len(triggered) != 1 || triggered[0] != "local:bge-small" {
		t.Errorf("expected reindex trigger for embedding model, got %+v", triggered)
	}

	// 非 embedding 模型不应触发
	triggered = nil
	if err := reg.DeprecateModel(ctx, "anthropic", "claude-3-opus-20240229", "claude-3-5-sonnet-latest"); err != nil {
		t.Fatalf("DeprecateModel (non-embedding): %v", err)
	}
	if len(triggered) != 0 {
		t.Errorf("expected no reindex trigger for non-embedding model, got %+v", triggered)
	}

	entry, err := reg.Get(ctx, "anthropic", "claude-3-opus-20240229")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry == nil || !entry.Deprecated || entry.SuccessorModelID != "claude-3-5-sonnet-latest" {
		t.Errorf("unexpected entry after deprecate: %+v", entry)
	}
}

func TestRecordCallResult_RollbackAfterThreeFailures(t *testing.T) {
	reg, db := newTestRegistry(t)
	ctx := context.Background()
	repoImpl := storerepo.NewSQLiteModelVersionRepository(db)

	// old(gpt-4) --successor--> new(gpt-4o-mini)
	if err := repoImpl.Upsert(ctx, &protorepo.ModelVersionEntry{
		ID: "openai:gpt-4", Provider: "openai", ModelID: "gpt-4",
		Deprecated: true, SuccessorModelID: "gpt-4o-mini", CompatibilityScore: 1,
	}); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := repoImpl.Upsert(ctx, &protorepo.ModelVersionEntry{
		ID: "openai:gpt-4o-mini", Provider: "openai", ModelID: "gpt-4o-mini", CompatibilityScore: 1,
	}); err != nil {
		t.Fatalf("seed new: %v", err)
	}

	for i := 0; i < 2; i++ {
		rollback, _, err := reg.RecordCallResult(ctx, "openai", "gpt-4o-mini", false)
		if err != nil {
			t.Fatalf("RecordCallResult: %v", err)
		}
		if rollback {
			t.Fatalf("should not rollback before threshold (i=%d)", i)
		}
	}

	rollback, rollbackTo, err := reg.RecordCallResult(ctx, "openai", "gpt-4o-mini", false)
	if err != nil {
		t.Fatalf("RecordCallResult: %v", err)
	}
	if !rollback {
		t.Fatal("expected rollback=true after 3 consecutive failures")
	}
	if rollbackTo != "gpt-4" {
		t.Errorf("expected rollback target gpt-4, got %q", rollbackTo)
	}

	// 一次成功应清零计数
	if _, _, err := reg.RecordCallResult(ctx, "openai", "gpt-4o-mini", true); err != nil {
		t.Fatalf("RecordCallResult (success): %v", err)
	}
	entry, err := reg.Get(ctx, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry.ConsecutiveErrors != 0 {
		t.Errorf("expected ConsecutiveErrors reset to 0, got %d", entry.ConsecutiveErrors)
	}
}

func TestRecordCallResult_UnregisteredModelIsNoop(t *testing.T) {
	reg, _ := newTestRegistry(t)
	rollback, rollbackTo, err := reg.RecordCallResult(context.Background(), "openai", "never-registered", false)
	if err != nil {
		t.Fatalf("RecordCallResult: %v", err)
	}
	if rollback || rollbackTo != "" {
		t.Errorf("expected no-op for unregistered model, got rollback=%v rollbackTo=%q", rollback, rollbackTo)
	}
}

func TestSeedFromStaticResolvers_IdempotentAndPopulates(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	if err := reg.SeedFromStaticResolvers(ctx); err != nil {
		t.Fatalf("SeedFromStaticResolvers: %v", err)
	}
	entry, err := reg.Get(ctx, "openai", "gpt-4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry == nil || entry.SuccessorModelID != "gpt-4o-mini" || !entry.Deprecated {
		t.Errorf("unexpected seeded entry: %+v", entry)
	}

	// 修改后再次 seed，不应覆盖（幂等）
	entry.CompatibilityScore = 0.3
	if err := reg.repo.Upsert(ctx, entry); err != nil {
		t.Fatalf("manual upsert: %v", err)
	}
	if err := reg.SeedFromStaticResolvers(ctx); err != nil {
		t.Fatalf("SeedFromStaticResolvers (second run): %v", err)
	}
	after, err := reg.Get(ctx, "openai", "gpt-4")
	if err != nil {
		t.Fatalf("Get after second seed: %v", err)
	}
	if after.CompatibilityScore != 0.3 {
		t.Errorf("expected seed to not overwrite existing entry, score changed to %v", after.CompatibilityScore)
	}
}

func TestSeedFromStaticResolvers_AllMappingsInsertable(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()
	if err := reg.SeedFromStaticResolvers(ctx); err != nil {
		t.Fatalf("SeedFromStaticResolvers: %v", err)
	}
	all, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != len(staticResolverMappings) {
		t.Errorf("expected %d seeded entries, got %d", len(staticResolverMappings), len(all))
	}
}
