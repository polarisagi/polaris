package marketplace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ── parseFrontmatter ──────────────────────────────────────────────────────────

func TestParseFrontmatter_Valid(t *testing.T) {
	content := []byte("---\nname: my-skill\ndescription: Does something useful\n---\n\n# Body")
	name, desc, err := parseFrontmatter(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my-skill" {
		t.Errorf("expected name=my-skill, got %q", name)
	}
	if desc != "Does something useful" {
		t.Errorf("expected description='Does something useful', got %q", desc)
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	_, _, err := parseFrontmatter([]byte("# Just a markdown file"))
	if err == nil {
		t.Fatal("expected error when no frontmatter found")
	}
}

func TestParseFrontmatter_MissingDescription(t *testing.T) {
	content := []byte("---\nname: tool\n---")
	name, desc, err := parseFrontmatter(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "tool" {
		t.Errorf("expected name=tool, got %q", name)
	}
	if desc != "" {
		t.Errorf("expected empty description, got %q", desc)
	}
}

func TestParseFrontmatter_OnlyClosingDelimiter(t *testing.T) {
	// 只有结束 --- 没有开头 --- → 报错
	_, _, err := parseFrontmatter([]byte("name: tool\n---"))
	if err == nil {
		t.Fatal("expected error when no opening ---")
	}
}

// ── SkillMetaFromSKILLmd ──────────────────────────────────────────────────────

func TestSkillMetaFromSKILLmd_Valid(t *testing.T) {
	tmp := t.TempDir()
	skillmd := filepath.Join(tmp, "SKILL.md")
	content := "---\nname: test-skill\ndescription: A test skill\n---\n\nSkill body here."
	os.WriteFile(skillmd, []byte(content), 0o644)

	meta, err := SkillMetaFromSKILLmd(skillmd, []byte("signing-key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Name != "skill:test-skill" {
		t.Errorf("expected Name='skill:test-skill', got %q", meta.Name)
	}
	if meta.Runtime != "markdown" {
		t.Errorf("expected Runtime='markdown', got %q", meta.Runtime)
	}
	if meta.Trust != protocol.TrustLocal {
		t.Errorf("expected TrustLocal, got %v", meta.Trust)
	}
	if meta.Sandbox != 1 {
		t.Errorf("expected Sandbox=1, got %d", meta.Sandbox)
	}
}

func TestSkillMetaFromSKILLmd_MissingName(t *testing.T) {
	tmp := t.TempDir()
	skillmd := filepath.Join(tmp, "SKILL.md")
	os.WriteFile(skillmd, []byte("---\ndescription: No name here\n---"), 0o644)

	_, err := SkillMetaFromSKILLmd(skillmd, []byte("key"))
	if err == nil {
		t.Fatal("expected error for missing name in frontmatter")
	}
}

func TestSkillMetaFromSKILLmd_NonExistentFile(t *testing.T) {
	_, err := SkillMetaFromSKILLmd("/nonexistent/SKILL.md", []byte("key"))
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

// ── LoadPlugin ────────────────────────────────────────────────────────────────

func makePlugin(t *testing.T, dir string, manifest protocol.PluginJSON) string {
	t.Helper()
	pluginDir := filepath.Join(dir, manifest.Name)
	codexDir := filepath.Join(pluginDir, ".codex-plugin")
	os.MkdirAll(codexDir, 0o755)

	data, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(codexDir, "plugin.json"), data, 0o644)
	return pluginDir
}

func TestLoadPlugin_Valid(t *testing.T) {
	tmp := t.TempDir()
	pluginDir := makePlugin(t, tmp, protocol.PluginJSON{
		Name:    "test-plugin",
		Version: "1.0.0",
	})

	p, err := LoadPlugin(pluginDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Manifest.Name != "test-plugin" {
		t.Errorf("expected Name='test-plugin', got %q", p.Manifest.Name)
	}
	if !p.Enabled {
		t.Error("loaded plugin should be Enabled=true by default")
	}
}

func TestLoadPlugin_MissingManifest(t *testing.T) {
	tmp := t.TempDir()
	_, err := LoadPlugin(filepath.Join(tmp, "no-plugin"))
	if err == nil {
		t.Fatal("expected error for missing manifest")
	}
}

func TestLoadPlugin_WithMCPConfig(t *testing.T) {
	tmp := t.TempDir()
	// plugin.json 引用 mcp.json
	manifest := protocol.PluginJSON{
		Name:       "plugin-with-mcp",
		Version:    "1.0.0",
		MCPServers: "mcp.json",
	}
	pluginDir := makePlugin(t, tmp, manifest)

	// 写入 mcp.json
	mcpConfig := protocol.MCPConfig{
		MCPServers: map[string]protocol.MCPServerDef{
			"my-server": {Command: "npx", Args: []string{"my-mcp"}},
		},
	}
	mcpData, _ := json.Marshal(mcpConfig)
	os.WriteFile(filepath.Join(pluginDir, "mcp.json"), mcpData, 0o644)

	p, err := LoadPlugin(pluginDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.MCPs) != 1 {
		t.Errorf("expected 1 MCP server, got %d", len(p.MCPs))
	}
	if _, ok := p.MCPs["my-server"]; !ok {
		t.Error("expected 'my-server' in MCPs")
	}
}

// ── Registry ──────────────────────────────────────────────────────────────────

func TestRegistry_Register_And_Get(t *testing.T) {
	r := NewRegistry()
	p := &Plugin{Manifest: protocol.PluginJSON{Name: "plugin-a"}, Enabled: true}

	if err := r.Register(p); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, ok := r.Get("plugin-a")
	if !ok {
		t.Fatal("expected plugin to be found")
	}
	if got.Manifest.Name != "plugin-a" {
		t.Errorf("expected plugin-a, got %q", got.Manifest.Name)
	}
}

func TestRegistry_Register_Duplicate_Errors(t *testing.T) {
	r := NewRegistry()
	p := &Plugin{Manifest: protocol.PluginJSON{Name: "dup"}}
	r.Register(p)

	err := r.Register(p)
	if err == nil {
		t.Fatal("duplicate registration should return error")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	r.Register(&Plugin{Manifest: protocol.PluginJSON{Name: "rm-me"}})
	r.Unregister("rm-me")

	_, ok := r.Get("rm-me")
	if ok {
		t.Error("unregistered plugin should not be found")
	}
}

func TestRegistry_SetEnabled(t *testing.T) {
	r := NewRegistry()
	r.Register(&Plugin{Manifest: protocol.PluginJSON{Name: "p1"}, Enabled: true})

	if err := r.SetEnabled("p1", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p, _ := r.Get("p1")
	if p.Enabled {
		t.Error("plugin should be disabled after SetEnabled(false)")
	}

	if err := r.SetEnabled("p1", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p, _ = r.Get("p1")
	if !p.Enabled {
		t.Error("plugin should be enabled after SetEnabled(true)")
	}
}

func TestRegistry_SetEnabled_NotFound(t *testing.T) {
	r := NewRegistry()
	if err := r.SetEnabled("ghost", true); err == nil {
		t.Error("SetEnabled on unknown plugin should return error")
	}
}

func TestRegistry_ListEnabled(t *testing.T) {
	r := NewRegistry()
	r.Register(&Plugin{Manifest: protocol.PluginJSON{Name: "on"}, Enabled: true})
	r.Register(&Plugin{Manifest: protocol.PluginJSON{Name: "off"}, Enabled: false})

	list := r.ListEnabled()
	if len(list) != 1 {
		t.Fatalf("expected 1 enabled plugin, got %d", len(list))
	}
	if list[0].Manifest.Name != "on" {
		t.Errorf("expected 'on', got %q", list[0].Manifest.Name)
	}
}

func TestRegistry_ScanDir_Empty(t *testing.T) {
	r := NewRegistry()
	tmp := t.TempDir()
	count, err := r.ScanDir(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("empty dir should yield 0 plugins, got %d", count)
	}
}

func TestRegistry_ScanDir_NonExistent(t *testing.T) {
	r := NewRegistry()
	count, err := r.ScanDir("/nonexistent/path/to/plugins")
	if err != nil {
		t.Fatalf("non-existent dir should not error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestRegistry_ScanDir_LoadsPlugins(t *testing.T) {
	r := NewRegistry()
	tmp := t.TempDir()

	makePlugin(t, tmp, protocol.PluginJSON{Name: "alpha", Version: "1.0.0"})
	makePlugin(t, tmp, protocol.PluginJSON{Name: "beta", Version: "2.0.0"})
	// 目录无 plugin.json → 跳过
	os.MkdirAll(filepath.Join(tmp, "no-manifest"), 0o755)

	count, err := r.ScanDir(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 plugins, got %d", count)
	}
}
