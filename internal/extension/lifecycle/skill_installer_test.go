package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

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

// writeSkillFixture 在临时目录下创建一个最小可安装的技能目录（含入口脚本）。
func writeSkillFixture(t *testing.T, scriptContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(scriptContent), 0o644); err != nil {
		t.Fatalf("failed to write fixture script: %v", err)
	}
	return dir
}

func TestSkillInstaller_Install_NoValidators_PreservesOldDefaults(t *testing.T) {
	dir := writeSkillFixture(t, "console.log('hi')")
	reg := &mockSkillRegistry{}
	inst := NewSkillInstaller(nil, reg)

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
	inst := NewSkillInstaller(nil, reg).WithValidators(
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
	inst := NewSkillInstaller(nil, reg).WithValidators(
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

func TestSkillInstaller_Install_NoEntryScript_SkipsValidationAndRegistration(t *testing.T) {
	dir := t.TempDir() // 无入口脚本
	reg := &mockSkillRegistry{}
	inst := NewSkillInstaller(nil, reg).WithValidators(
		&stubAnalyzer{passed: false}, // 若被误调用会导致本用例失败
		&stubRiskAssessor{},
	)

	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_noscript", LocalPath: dir}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg.registered != nil {
		t.Fatal("expected no registration when skill has no entry script")
	}
}
