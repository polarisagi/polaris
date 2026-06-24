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

type mockScriptLoader struct {
	path string
	err  error
}

func (m *mockScriptLoader) LoadScriptPath(skillID string) (string, error) {
	return m.path, m.err
}

func TestRegistryImpl_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	meta := types.SkillMeta{
		Name:    "skill:test",
		Version: "1.0",
		Trust:   types.TrustLocal,
	}

	if err := reg.Register(ctx, meta); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	got, err := reg.Get(ctx, "skill:test", "1.0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Name != "skill:test" {
		t.Errorf("expected skill:test")
	}

	// Collision
	if err := reg.Register(ctx, meta); err == nil {
		t.Errorf("expected collision err")
	}

	// Invalid name
	meta.Name = "invalid-name"
	if err := reg.Register(ctx, meta); err == nil {
		t.Errorf("expected name error")
	}

	// Invalid trust
	meta.Name = "skill:untrusted"
	meta.Trust = 0
	if err := reg.Register(ctx, meta); err == nil {
		t.Errorf("expected trust error")
	}
}

func TestRegistryImpl_DeprecateAndList(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	meta1 := types.SkillMeta{Name: "skill:test1", Version: "1.0", Trust: types.TrustLocal, Capabilities: []string{"write"}}
	meta2 := types.SkillMeta{Name: "skill:test2", Version: "1.0", Trust: types.TrustLocal, Capabilities: []string{"read"}}

	_ = reg.Register(ctx, meta1)
	_ = reg.Register(ctx, meta2)

	// List
	list, _ := reg.List(ctx, types.SkillFilter{})
	if len(list) != 2 {
		t.Errorf("expected 2")
	}

	list, _ = reg.List(ctx, types.SkillFilter{Capabilities: []string{"write"}})
	if len(list) != 1 {
		t.Errorf("expected 1")
	}

	// Deprecate
	_ = reg.Deprecate(ctx, "skill:test1", "", "old")
	list, _ = reg.List(ctx, types.SkillFilter{IncludeDeprecated: false})
	if len(list) != 1 {
		t.Errorf("expected 1 after deprecation")
	}
	list, _ = reg.List(ctx, types.SkillFilter{IncludeDeprecated: true})
	if len(list) != 2 {
		t.Errorf("expected 2 with IncludeDeprecated")
	}

	logs := reg.AuditLog()
	if len(logs) != 1 || !strings.Contains(logs[0], "deprecate skill:test1") {
		t.Errorf("expected audit log")
	}
}

func TestSelectorImpl_Select(t *testing.T) {
	reg := NewRegistry()
	ctx := context.Background()

	_ = reg.Register(ctx, types.SkillMeta{Name: "skill:cap1", Capabilities: []string{"cap1"}, Trust: types.TrustLocal, RiskLevel: "low", Benchmarks: types.SkillBenchmarks{PassRate: 0.9}})
	_ = reg.Register(ctx, types.SkillMeta{Name: "skill:cap2", Capabilities: []string{"cap2"}, Trust: types.TrustLocal, RiskLevel: "high"})

	selector := NewSelector(reg)
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
	reg := NewRegistry()
	ctx := context.Background()

	meta := types.SkillMeta{
		Name:       "skill:exec",
		Version:    "1.0",
		Trust:      types.TrustLocal,
		ScriptPath: "/path/to/script",
	}
	_ = reg.Register(ctx, meta)

	runner := &mockScriptRunner{response: []byte("success")}
	loader := &mockScriptLoader{}

	exec := NewScriptSkillExecutor(reg, runner, loader)
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
