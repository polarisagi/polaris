package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// mockSkillRegistry 记录 Register 调用，满足 protocol.SkillRegistry 接口。
type mockSkillRegistry struct {
	registered []types.SkillMeta
}

func (m *mockSkillRegistry) Register(_ context.Context, meta types.SkillMeta) error {
	m.registered = append(m.registered, meta)
	return nil
}

func (m *mockSkillRegistry) Get(_ context.Context, _, _ string) (*types.SkillMeta, error) {
	return nil, nil
}

func (m *mockSkillRegistry) List(_ context.Context, _ types.SkillFilter) ([]types.SkillMeta, error) {
	return nil, nil
}

func (m *mockSkillRegistry) Deprecate(_ context.Context, _, _, _ string) error { return nil }

// TestRuntimeRegistrarAdapter_Skill_Happy 验证 skill 扩展正确注册到 SkillRegistry。
func TestRuntimeRegistrarAdapter_Skill_Happy(t *testing.T) {
	// 构造临时 installDir，创建 src/index.ts 和 SKILL.md
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# My Skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	sr := &mockSkillRegistry{}
	reg := &runtimeRegistrarAdapter{skillRegistry: sr}

	err := reg.Register(context.Background(), "skill", dir, "ext_abc123")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if len(sr.registered) != 1 {
		t.Fatalf("expected 1 skill registered, got %d", len(sr.registered))
	}
	if sr.registered[0].ScriptPath != filepath.Join(dir, "src/index.ts") {
		t.Errorf("unexpected ScriptPath: %s", sr.registered[0].ScriptPath)
	}
	if sr.registered[0].Instructions != "# My Skill" {
		t.Errorf("unexpected Instructions: %q", sr.registered[0].Instructions)
	}
	if sr.registered[0].Name != "abc123" {
		t.Errorf("unexpected Name (expected ext_ prefix stripped): %s", sr.registered[0].Name)
	}
}

// TestRuntimeRegistrarAdapter_EmptyInstallDir_NoError 验证 installDir 为空时静默降级不报错。
func TestRuntimeRegistrarAdapter_EmptyInstallDir_NoError(t *testing.T) {
	reg := &runtimeRegistrarAdapter{skillRegistry: &mockSkillRegistry{}}
	if err := reg.Register(context.Background(), "skill", "", "ext_xyz"); err != nil {
		t.Errorf("empty installDir should not return error, got: %v", err)
	}
}

// TestRuntimeRegistrarAdapter_MCP_MissingManifest_NoError 验证 mcp.json 缺失时静默降级不报错。
func TestRuntimeRegistrarAdapter_MCP_MissingManifest_NoError(t *testing.T) {
	dir := t.TempDir() // 空目录，没有 mcp.json
	reg := &runtimeRegistrarAdapter{}
	if err := reg.Register(context.Background(), "mcp", dir, "ext_mcp1"); err != nil {
		t.Errorf("missing mcp.json should not return error, got: %v", err)
	}
}

// TestRuntimeRegistrarAdapter_UnknownExtType_NoError 验证未知 extType 静默跳过不报错。
func TestRuntimeRegistrarAdapter_UnknownExtType_NoError(t *testing.T) {
	reg := &runtimeRegistrarAdapter{}
	if err := reg.Register(context.Background(), "plugin", "/some/dir", "ext_p1"); err != nil {
		t.Errorf("unknown extType should not return error, got: %v", err)
	}
}
