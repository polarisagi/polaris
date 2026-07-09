package marketplace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestParseManifestDir_AIPlugin(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"name_for_human": "Test Plugin",
		"description_for_human": "Test Description",
		"api": {
			"type": "openapi",
			"url": "http://localhost/openapi.json"
		},
		"legal_info_url": "http://localhost/legal"
	}`
	err := os.WriteFile(filepath.Join(dir, "ai-plugin.json"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp", Publisher: "test_pub", TrustTier: 3}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Name != "Test Plugin" || e.Description != "Test Description" || e.Type != "app" || e.Transport != "" || e.URL != "http://localhost/openapi.json" {
		t.Errorf("unexpected entry: %+v", e)
	}
}

func TestParseManifestDir_AIPlugin_MCP(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"name_for_model": "Test Model",
		"description_for_model": "Test Model Desc",
		"api": {
			"type": "mcp",
			"url": "http://localhost/mcp"
		}
	}`
	err := os.WriteFile(filepath.Join(dir, "ai-plugin.json"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp", Publisher: "test_pub", TrustTier: 3}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Name != "Test Model" || e.Type != "mcp" || e.Transport != "http" || e.URL != "http://localhost/mcp" {
		t.Errorf("unexpected entry: %+v", e)
	}
}

func TestParseManifestDir_AnthropicTOML(t *testing.T) {
	dir := t.TempDir()
	content := `
[plugin]
name = "Anthropic Test"
description = "Desc"

[mcp]
command = "python"
args = ["-m", "mcp"]
`
	err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp", Publisher: "test_pub", TrustTier: 3}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Name != "Anthropic Test" || e.Type != "mcp" || e.Command != "python" || len(e.Args) != 2 {
		t.Errorf("unexpected entry: %+v", e)
	}
}

func TestParseManifestDir_ClaudePluginJSON(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude-plugin")
	err := os.Mkdir(claudeDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	content := `{
		"name": "Claude Test",
		"description": "Desc",
		"interface": {
			"displayName": "Display Name"
		}
	}`
	err = os.WriteFile(filepath.Join(claudeDir, "plugin.json"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp", Publisher: "test_pub", TrustTier: 3}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Name != "Claude Test" || e.Type != "plugin" || e.DisplayName != "Display Name" {
		t.Errorf("unexpected entry: %+v", e)
	}
}

func TestParseManifestDir_AppJSON(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"apps": [
			{"name": "App1", "command": "cmd1"},
			{"name": "App2", "url": "url2"}
		]
	}`
	err := os.WriteFile(filepath.Join(dir, ".app.json"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp"}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name != "App1" || entries[0].Command != "cmd1" || entries[0].Type != "app" {
		t.Errorf("unexpected entry 0: %+v", entries[0])
	}
	if entries[1].Name != "App2" || entries[1].URL != "url2" || entries[1].Type != "app" {
		t.Errorf("unexpected entry 1: %+v", entries[1])
	}
}

func TestParseManifestDir_GoogleYAML(t *testing.T) {
	dir := t.TempDir()
	content1 := `
name: Single Skill
command: single_cmd
`
	err := os.WriteFile(filepath.Join(dir, "skills.yaml"), []byte(content1), 0644)
	if err != nil {
		t.Fatal(err)
	}

	content2 := `
skills:
  - name: Multi Skill 1
    command: multi_cmd1
  - name: Multi Skill 2
`
	err = os.WriteFile(filepath.Join(dir, "agent-manifest.yaml"), []byte(content2), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp"}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	// order is skills.yaml then agent-manifest.yaml
	if entries[0].Name != "Single Skill" || entries[0].Type != "mcp" || entries[0].Command != "single_cmd" {
		t.Errorf("unexpected entry 0: %+v", entries[0])
	}
	if entries[1].Name != "Multi Skill 1" || entries[1].Type != "mcp" || entries[1].Command != "multi_cmd1" {
		t.Errorf("unexpected entry 1: %+v", entries[1])
	}
	if entries[2].Name != "Multi Skill 2" || entries[2].Type != "skill" || entries[2].Command != "" {
		t.Errorf("unexpected entry 2: %+v", entries[2])
	}
}

func TestParseManifestDir_PackageJSON(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"name": "my-mcp-server",
		"description": "NPM package",
		"dependencies": {
			"@modelcontextprotocol/sdk": "1.0.0"
		}
	}`
	err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp"}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "my-mcp-server" || entries[0].Type != "mcp" || entries[0].Command != "npx" {
		t.Errorf("unexpected entry: %+v", entries[0])
	}
}

func TestParseManifestDir_PyProjectTOML(t *testing.T) {
	dir := t.TempDir()
	content := `
[project]
name = "my-mcp-server"
description = "Py package"
dependencies = ["mcp"]
`
	err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	mp := protocol.Marketplace{ID: "test_mp"}
	entries, err := ParseManifestDir(dir, dir, mp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "my-mcp-server" || entries[0].Type != "mcp" || entries[0].Command != "uvx" {
		t.Errorf("unexpected entry: %+v", entries[0])
	}
}
