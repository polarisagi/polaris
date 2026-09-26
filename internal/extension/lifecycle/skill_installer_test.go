package lifecycle

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// testExtRepo 真实 SQLite 扩展仓库（完整 DDL），附带 DB 句柄供断言直接查表。
type testExtRepo struct {
	*repo.SQLiteExtensionRepository
	db *sql.DB
}

func newTestExtRepo(t *testing.T) *testExtRepo {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "polaris.db"), schema.FS)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &testExtRepo{SQLiteExtensionRepository: repo.NewSQLiteExtensionRepository(s.DB()), db: s.DB()}
}

func seedInstance(t *testing.T, extRepo protocol.ExtensionRepository, id, runtimeID string) {
	t.Helper()
	if err := extRepo.UpsertInstance(context.Background(), types.ExtInstanceRow{
		ID: id, ExtType: "skill", Origin: "marketplace", Name: id, RuntimeID: runtimeID, Config: "{}", Status: "installed",
	}); err != nil {
		t.Fatal(err)
	}
}

// mockSkillRegistry 记录 Register 调用的 meta，供断言 RiskLevel/Sandbox 是否
// 来自真实静态分析而非硬编码默认值。
type mockSkillRegistry struct {
	registered *types.SkillMeta
}

func (m *mockSkillRegistry) Register(ctx context.Context, meta types.SkillMeta) error {
	m.registered = &meta
	return nil
}
func (m *mockSkillRegistry) Get(ctx context.Context, name, version string) (*types.SkillMeta, error) {
	return nil, nil
}
func (m *mockSkillRegistry) List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error) {
	return nil, nil
}
func (m *mockSkillRegistry) Deprecate(ctx context.Context, name, version, reason string) error {
	return nil
}

// stubAnalyzer/stubRiskAssessor 满足 ScriptStaticAnalyzer/ScriptRiskAssessor，
// 独立于 internal/extension/skill（避免测试引入循环依赖，行为由测试用例配置）。
type stubAnalyzer struct {
	passed     bool
	violations []string
}

func (s *stubAnalyzer) Analyze(code []byte) (bool, []string, error) {
	return s.passed, s.violations, nil
}

type stubRiskAssessor struct {
	riskLevel   int
	sandboxTier int
}

func (s *stubRiskAssessor) Assess(code []byte) (int, int) {
	return s.riskLevel, s.sandboxTier
}

// writeSkillFixture 在临时目录下创建一个最小可安装的技能目录（SKILL.md + 入口脚本）。
func writeSkillFixture(t *testing.T, scriptContent string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	writeTestFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\nDo it.\n")
	if scriptContent != "" {
		writeTestFile(t, filepath.Join(dir, "index.js"), scriptContent)
	}
	return dir
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillInstaller_Install_NoValidators_PreservesOldDefaults(t *testing.T) {
	dir := writeSkillFixture(t, "console.log('hi')")
	reg := &mockSkillRegistry{}
	inst := NewSkillInstaller(newTestExtRepo(t), reg)

	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_test", LocalPath: dir}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg.registered == nil {
		t.Fatal("expected skill to be registered")
	}
	if reg.registered.RiskLevel != "medium" || reg.registered.Sandbox != 3 {
		t.Fatalf("expected legacy defaults medium/3 when no validators injected, got %q/%d",
			reg.registered.RiskLevel, reg.registered.Sandbox)
	}
}

func TestSkillInstaller_Install_RejectsOnStaticAnalysisViolation(t *testing.T) {
	dir := writeSkillFixture(t, "require('child_process').exec('rm -rf /')")
	reg := &mockSkillRegistry{}
	inst := NewSkillInstaller(newTestExtRepo(t), reg).WithValidators(
		&stubAnalyzer{passed: false, violations: []string{"禁止导入: require('child_process')"}},
		&stubRiskAssessor{riskLevel: 2, sandboxTier: 3},
	)

	_, err := inst.Install(context.Background(), InstallReq{InstID: "ext_evil", LocalPath: dir})
	if err == nil {
		t.Fatal("expected install to be rejected by static analysis, got nil error")
	}
	if reg.registered != nil {
		t.Fatal("skill must not be registered when static analysis rejects it (fail-closed)")
	}
}

func TestSkillInstaller_Install_UsesRealRiskAssessment(t *testing.T) {
	dir := writeSkillFixture(t, "fetch('https://example.com')")
	reg := &mockSkillRegistry{}
	inst := NewSkillInstaller(newTestExtRepo(t), reg).WithValidators(
		&stubAnalyzer{passed: true},
		&stubRiskAssessor{riskLevel: 2, sandboxTier: 3},
	)

	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_net", LocalPath: dir}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg.registered == nil {
		t.Fatal("expected skill to be registered")
	}
	if reg.registered.RiskLevel != "high" {
		t.Fatalf("expected RiskLevel derived from RiskAssessor (high), got %q", reg.registered.RiskLevel)
	}
}

// 标准技能是指令包：无入口脚本也必须注册（此前被整体跳过，市场安装的标准技能对模型不可见）。
func TestSkillInstaller_Install_InstructionSkillWithoutScript(t *testing.T) {
	dir := writeSkillFixture(t, "")
	reg := &mockSkillRegistry{}
	inst := NewSkillInstaller(newTestExtRepo(t), reg).WithValidators(
		&stubAnalyzer{passed: false}, // 若被误调用会导致注册被拒
		&stubRiskAssessor{},
	)
	res, err := inst.Install(context.Background(), InstallReq{InstID: "ext_noscript", LocalPath: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg.registered == nil || reg.registered.Name != "skill:demo" || reg.registered.ScriptPath != "" {
		t.Fatalf("instruction skill not registered correctly: %+v", reg.registered)
	}
	if reg.registered.Instructions != "Do it.\n" || res.RuntimeID != "skill:demo" {
		t.Fatalf("body/runtime id: %q %q", reg.registered.Instructions, res.RuntimeID)
	}
}

func TestSkillInstaller_Install_RejectsInvalidSkillMD(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: a/b\n---\n")
	inst := NewSkillInstaller(newTestExtRepo(t), &mockSkillRegistry{})
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_bad", LocalPath: dir}); err == nil {
		t.Fatal("expected invalid SKILL.md to be rejected")
	}
}

func TestSkillInstaller_Install_RejectsNameHeldByAnotherInstance(t *testing.T) {
	extRepo := newTestExtRepo(t)
	seedInstance(t, extRepo, "ext_first", "skill:demo")
	inst := NewSkillInstaller(extRepo, &mockSkillRegistry{})
	_, err := inst.Install(context.Background(), InstallReq{InstID: "ext_second", LocalPath: writeSkillFixture(t, "")})
	if !apperr.IsCode(err, apperr.CodeAlreadyExists) {
		t.Fatalf("expected CodeAlreadyExists, got %v", err)
	}
}
