package skill

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestSQLiteRegistry(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS skills (
			name TEXT PRIMARY KEY,
			version TEXT,
			runtime TEXT,
			risk_level TEXT,
			sandbox TEXT,
			capabilities TEXT,
			exec_mode TEXT,
			ambient_priority TEXT,
			trust_tier INTEGER,
			idempotent BOOLEAN,
			benchmarks TEXT,
			instructions TEXT,
			deprecated BOOLEAN,
			depends_on TEXT,
			composes_of TEXT,
			plugin_id TEXT,
			needs_compat_check INTEGER,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS extension_instances (
			runtime_id TEXT,
			ext_type TEXT,
			install_path TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	reg := NewSQLiteRegistry(db)

	ctx := context.Background()

	meta := types.SkillMeta{
		Name:         "skill:sqlite",
		Version:      "1.0",
		Trust:        types.TrustLocal,
		Capabilities: []string{"read"},
	}

	// Register
	err = reg.Register(ctx, meta)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// Get
	got, err := reg.Get(ctx, "skill:sqlite", "1.0")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Name != "skill:sqlite" {
		t.Errorf("expected skill:sqlite, got %s", got.Name)
	}

	// List
	list, err := reg.List(ctx, types.SkillFilter{Capabilities: []string{"read"}})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 skill, got %d", len(list))
	}

	// Deprecate
	err = reg.Deprecate(ctx, "skill:sqlite", "1.0", "obsolete")
	if err != nil {
		t.Fatalf("deprecate failed: %v", err)
	}

	got2, err := reg.Get(ctx, "skill:sqlite", "1.0")
	if err != nil {
		t.Fatalf("get after deprecate failed: %v", err)
	}
	if !got2.Deprecated {
		t.Errorf("expected skill to be deprecated")
	}

	// Cycle detection during Register
	meta2 := types.SkillMeta{
		Name:      "skill:cycle1",
		Trust:     types.TrustLocal,
		DependsOn: []string{"skill:cycle2"},
	}
	_ = reg.Register(ctx, meta2)

	meta3 := types.SkillMeta{
		Name:       "skill:cycle2",
		Trust:      types.TrustLocal,
		ComposesOf: []string{"skill:cycle1"},
	}
	err = reg.Register(ctx, meta3)
	if err == nil {
		t.Errorf("expected cyclic dependency error")
	}
}
