package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

type mockScriptRunner struct {
	response []byte
	err      error
}

func (m *mockScriptRunner) RunScript(ctx context.Context, skillName string, scriptPath string, input []byte, trustTier types.TrustTier) ([]byte, error) {
	return m.response, m.err
}

// allowAllPolicyGate 测试用永久放行 PolicyGate（实现 protocol.PolicyGate）。
type allowAllPolicyGate struct{}

func (allowAllPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return true, nil
}

func (allowAllPolicyGate) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

type mockScriptLoader struct {
	path string
	err  error
}

func (m *mockScriptLoader) LoadScriptPath(skillID string) (string, error) {
	return m.path, m.err
}

// 2026-07-14（ADR-0051）：内存版 RegistryImpl 专属测试
// （TestRegistryImpl_RegisterAndGet/_UpgradeMarksReverseDependencies/
// _DeprecateAndList）随 RegistryImpl 一并删除——Register/Get/collision/
// 升级反向依赖标记/List(IncludeDeprecated)/AuditLog 的等价覆盖已移植到
// sqlite_registry_test.go（TestSQLiteRegistry/
// TestSQLiteRegistry_UpgradeMarksReverseDependencies/
// TestSQLiteRegistry_ListIncludeDeprecated），针对当前唯一生产实现
// SQLiteRegistryImpl，而非已删除的内存测试替身。

func TestSelectorImpl_Select(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()

	_ = reg.Register(ctx, types.SkillMeta{Name: "skill:cap1", Capabilities: []string{"cap1"}, Trust: types.TrustLocal, RiskLevel: "low", Benchmarks: types.SkillBenchmarks{PassRate: 0.9}})
	_ = reg.Register(ctx, types.SkillMeta{Name: "skill:cap2", Capabilities: []string{"cap2"}, Trust: types.TrustLocal, RiskLevel: "high"})

	selector := NewHybridRetriever(reg, nil, nil)
	hints := types.TaskHint{CapabilitiesNeeded: []string{"cap1"}, ComplexityScore: 0.9}

	skills, err := selector.Select(ctx, hints)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if len(skills) == 0 {
		t.Fatalf("expected skills")
	}
	if skills[0].Name != "skill:cap1" {
		t.Errorf("expected cap1 first")
	}
}

func TestScriptSkillExecutor_ExecuteSkill(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()

	meta := types.SkillMeta{
		Name:    "skill:exec",
		Version: "1.0",
		Trust:   types.TrustLocal,
	}
	_ = reg.Register(ctx, meta)

	// SQLiteRegistryImpl.Get 只从 extension_instances.install_path 派生
	// ScriptPath（marketplace 安装路径），Register 不落盘裸 meta.ScriptPath
	// 字段（2026-07-14 由内存版 RegistryImpl 切换到 SQLiteRegistryImpl 后的真实
	// 行为差异，ADR-0051）。本用例走文件系统兜底加载器（loader.LoadScriptPath）
	// 这条生产同样支持的路径来提供脚本路径。
	runner := &mockScriptRunner{response: []byte("success")}
	loader := &mockScriptLoader{path: "/path/to/script"}

	exec := NewScriptSkillExecutor(reg, runner, loader).WithPolicy(allowAllPolicyGate{})
	resp, err := exec.ExecuteSkill(ctx, "skill:exec", []byte("input"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(resp) != "success" {
		t.Errorf("expected success")
	}

	// Test Deprecated
	_ = reg.Deprecate(ctx, "skill:exec", "", "deprecated")
	_, err = exec.ExecuteSkill(ctx, "skill:exec", []byte("input"))
	if err == nil {
		t.Errorf("expected deprecate error")
	}
}

// TestScriptSkillExecutor_ExecuteSkill_NoScriptReturnsInstructions 验证纯 SKILL.md
// 指令技能（无 ScriptPath）不再回显原始输入，而是返回 instructions 全文，
// 与 cmd/polaris/skill_loader.go 注册的同名工具语义一致（唯一实现）。
func TestScriptSkillExecutor_ExecuteSkill_NoScriptReturnsInstructions(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()

	meta := types.SkillMeta{
		Name:         "skill:instructiononly",
		Version:      "1.0",
		Trust:        types.TrustLocal,
		Instructions: "请先读取文件再总结要点",
	}
	_ = reg.Register(ctx, meta)

	// runner 非 nil 但没有 ScriptPath 可用 → 应落到 instructions 分支，不调用 runner。
	runner := &mockScriptRunner{response: []byte("should-not-be-called")}
	exec := NewScriptSkillExecutor(reg, runner, nil).WithPolicy(allowAllPolicyGate{})

	resp, err := exec.ExecuteSkill(ctx, "skill:instructiononly", []byte(`{"input":"目标文件 a.txt"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got := string(resp)
	if !strings.Contains(got, "请先读取文件再总结要点") || !strings.Contains(got, "目标文件 a.txt") {
		t.Errorf("expected instructions + input echo, got: %q", got)
	}
	if strings.Contains(got, "should-not-be-called") {
		t.Errorf("runner must not be invoked when there is no script to run")
	}
}

// TestScriptSkillExecutor_ExecuteSkill_FailClosedWithoutPolicy 验证脚本执行前
// 未配置 PolicyGate 时 fail-closed 拒绝，而不是静默放行执行脚本（R1.14）。
func TestScriptSkillExecutor_ExecuteSkill_FailClosedWithoutPolicy(t *testing.T) {
	reg := newTestSQLiteRegistry(t)
	ctx := context.Background()

	meta := types.SkillMeta{
		Name:    "skill:noPolicy",
		Version: "1.0",
		Trust:   types.TrustLocal,
	}
	_ = reg.Register(ctx, meta)

	// SQLiteRegistryImpl.Get 只从 extension_instances.install_path 派生
	// ScriptPath（Register 不落盘裸 meta.ScriptPath 字段，ADR-0051），本用例
	// 直接写入 extension_instances 模拟 marketplace 安装路径，使 scriptPath
	// 非空从而真正触达下方待测的 fail-closed 分支。
	_, err := reg.db.ExecContext(ctx,
		"INSERT INTO extension_instances (runtime_id, ext_type, install_path) VALUES (?, 'skill', ?)",
		"skill:noPolicy", "/path/to")
	if err != nil {
		t.Fatalf("seed extension_instances: %v", err)
	}

	runner := &mockScriptRunner{response: []byte("should-not-run")}
	exec := NewScriptSkillExecutor(reg, runner, nil) // 未调用 WithPolicy

	_, err = exec.ExecuteSkill(ctx, "skill:noPolicy", []byte("input"))
	if err == nil {
		t.Fatalf("expected fail-closed error when policy gate not configured")
	}
}
