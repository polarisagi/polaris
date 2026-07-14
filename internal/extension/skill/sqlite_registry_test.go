package skill

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/pkg/types"
)

// newTestSQLiteRegistry 创建一个内存 SQLite 支撑的 SQLiteRegistryImpl，供本包
// 各测试文件共用（skill_test.go 的选择器/执行器测试、本文件的 Registry 行为测试）。
// 2026-07-14：内存版 RegistryImpl 删除后（ADR-0051），SQLiteRegistryImpl 是
// protocol.SkillRegistry 的唯一实现，测试 fixture 统一改用它，与生产实际使用的
// 类型保持一致（而非维护一个生产早已不用的内存测试替身）。
func newTestSQLiteRegistry(t *testing.T) *SQLiteRegistryImpl {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

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
	return NewSQLiteRegistry(db)
}

func TestSQLiteRegistry(t *testing.T) {
	reg := newTestSQLiteRegistry(t)

	ctx := context.Background()

	meta := types.SkillMeta{
		Name:         "skill:sqlite",
		Version:      "1.0",
		Trust:        types.TrustLocal,
		Capabilities: []string{"read"},
	}

	// Register
	err := reg.Register(ctx, meta)
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

// TestSQLiteRegistry_UpgradeMarksReverseDependencies 验证 SQLiteRegistryImpl
// 在技能升级（同名不同版本）时会对其反向依赖方（DependsOn/ComposesOf 引用了被
// 升级技能的其他技能）标记 needs_compat_check=1，含传递依赖场景。
// 2026-07-14（ADR-0051）：从已删除的内存版 RegistryImpl 同名测试移植，避免删除
// 内存版实现时丢失这条真实覆盖（markReverseDependenciesCompatCheck 反向 BFS）。
func TestSQLiteRegistry_UpgradeMarksReverseDependencies(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()

	base := types.SkillMeta{Name: "skill:base", Version: "1.0", Trust: types.TrustLocal}
	if err := reg.Register(ctx, base); err != nil {
		t.Fatalf("register base: %v", err)
	}

	dependent := types.SkillMeta{
		Name: "skill:dependent", Version: "1.0", Trust: types.TrustLocal,
		DependsOn: []string{"skill:base"},
	}
	if err := reg.Register(ctx, dependent); err != nil {
		t.Fatalf("register dependent: %v", err)
	}

	transitive := types.SkillMeta{
		Name: "skill:transitive", Version: "1.0", Trust: types.TrustLocal,
		ComposesOf: []string{"skill:dependent"},
	}
	if err := reg.Register(ctx, transitive); err != nil {
		t.Fatalf("register transitive: %v", err)
	}

	// 同名不同版本 = 升级，不应报 collision 错误。
	upgraded := types.SkillMeta{Name: "skill:base", Version: "2.0", Trust: types.TrustLocal}
	if err := reg.Register(ctx, upgraded); err != nil {
		t.Fatalf("upgrade should succeed, got err: %v", err)
	}

	got, err := reg.Get(ctx, "skill:base", "")
	if err != nil {
		t.Fatalf("get base: %v", err)
	}
	if got.Version != "2.0" {
		t.Errorf("expected upgraded version 2.0, got %s", got.Version)
	}

	dep, err := reg.Get(ctx, "skill:dependent", "")
	if err != nil {
		t.Fatalf("get dependent: %v", err)
	}
	if !dep.NeedsCompatCheck {
		t.Errorf("expected skill:dependent.NeedsCompatCheck=true after skill:base upgrade")
	}

	trans, err := reg.Get(ctx, "skill:transitive", "")
	if err != nil {
		t.Fatalf("get transitive: %v", err)
	}
	if !trans.NeedsCompatCheck {
		t.Errorf("expected skill:transitive.NeedsCompatCheck=true (transitive via skill:dependent) after skill:base upgrade")
	}
}

// TestSQLiteRegistry_ListIncludeDeprecated 验证 List 的 IncludeDeprecated
// 过滤开关：默认排除已废弃技能，显式请求时包含。
// 2026-07-14（ADR-0051）：从已删除的内存版 RegistryImpl 同名测试移植。
func TestSQLiteRegistry_ListIncludeDeprecated(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()

	meta1 := types.SkillMeta{Name: "skill:test1", Version: "1.0", Trust: types.TrustLocal, Capabilities: []string{"write"}}
	meta2 := types.SkillMeta{Name: "skill:test2", Version: "1.0", Trust: types.TrustLocal, Capabilities: []string{"read"}}
	_ = reg.Register(ctx, meta1)
	_ = reg.Register(ctx, meta2)

	_ = reg.Deprecate(ctx, "skill:test1", "", "old")

	list, _ := reg.List(ctx, types.SkillFilter{IncludeDeprecated: false})
	if len(list) != 1 {
		t.Errorf("expected 1 after deprecation, got %d", len(list))
	}
	list, _ = reg.List(ctx, types.SkillFilter{IncludeDeprecated: true})
	if len(list) != 2 {
		t.Errorf("expected 2 with IncludeDeprecated, got %d", len(list))
	}
}
