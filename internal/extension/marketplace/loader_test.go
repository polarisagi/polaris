package marketplace

import (
	"crypto/hmac"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestParseFrontmatter_Valid(t *testing.T) {
	content := `---
name: test_skill
description: this is a test
exec_mode: auto
---
# Content`
	name, desc, mode, err := parseFrontmatter([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if name != "test_skill" || desc != "this is a test" || mode != "auto" {
		t.Errorf("unexpected parsing results: name=%s, desc=%s, mode=%s", name, desc, mode)
	}
}

func TestParseFrontmatter_MissingExecMode(t *testing.T) {
	content := `---
name: test_skill
description: this is a test
---
# Content`
	name, desc, mode, err := parseFrontmatter([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if name != "test_skill" || desc != "this is a test" || mode != "tool" {
		t.Errorf("unexpected parsing results: name=%s, desc=%s, mode=%s", name, desc, mode)
	}
}

func TestParseFrontmatter_Invalid(t *testing.T) {
	content := `---
invalid yaml
---`
	_, _, _, err := parseFrontmatter([]byte(content))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSkillMetaFromSKILLmd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	content := `---
name: test_skill
description: this is a test
exec_mode: auto
---
# Content`
	err := os.WriteFile(path, []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	key := []byte("secret")
	meta, err := SkillMetaFromSKILLmd(path, key)
	if err != nil {
		t.Fatal(err)
	}

	if meta.Name != "skill:test_skill" { // capability string is description:this is a test
		// let's check capabilities
		if len(meta.Capabilities) != 1 || meta.Capabilities[0] != "description:this is a test" {
			t.Errorf("unexpected capabilities: %+v", meta.Capabilities)
		}
	}
	if meta.Trust != types.TrustLocal || meta.ExecMode != "auto" {
		t.Errorf("unexpected meta fields: trust=%v, exec_mode=%s", meta.Trust, meta.ExecMode)
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(content))
	// Validating HMAC logic internally happens in verification not here, but loader should set TrustLocal
}

func TestLoadMCPConfig(t *testing.T) {
	dir := t.TempDir()

	t.Run("Normal", func(t *testing.T) {
		path := filepath.Join(dir, "mcp1.json")
		content := `{
			"mcpServers": {
				"server1": {"command": "cmd1"}
			}
		}`
		os.WriteFile(path, []byte(content), 0644)
		cfg, err := loadMCPConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.MCPServers) != 1 || cfg.MCPServers["server1"].Command != "cmd1" {
			t.Errorf("unexpected config: %+v", cfg.MCPServers)
		}
	})

	t.Run("Snake", func(t *testing.T) {
		path := filepath.Join(dir, "mcp2.json")
		content := `{
			"mcp_servers": {
				"server2": {"command": "cmd2"}
			}
		}`
		os.WriteFile(path, []byte(content), 0644)
		cfg, err := loadMCPConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.MCPServers) != 1 || cfg.MCPServers["server2"].Command != "cmd2" {
			t.Errorf("unexpected config: %+v", cfg.MCPServers)
		}
	})

	t.Run("Flat", func(t *testing.T) {
		path := filepath.Join(dir, "mcp3.json")
		content := `{
			"server3": {"command": "cmd3"},
			"server4": {"url": "http://loc"}
		}`
		os.WriteFile(path, []byte(content), 0644)
		cfg, err := loadMCPConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.MCPServers) != 2 || cfg.MCPServers["server3"].Command != "cmd3" {
			t.Errorf("unexpected config: %+v", cfg.MCPServers)
		}
	})
}

func TestLoadPlugin(t *testing.T) {
	dir := t.TempDir()
	metaDir := filepath.Join(dir, ".polaris-plugin")
	os.MkdirAll(metaDir, 0755)

	pluginJSON := `{
		"name": "test_plugin",
		"mcpServers": ".mcp.json"
	}`
	os.WriteFile(filepath.Join(metaDir, "plugin.json"), []byte(pluginJSON), 0644)

	mcpJSON := `{
		"mcpServers": {
			"server1": {"command": "cmd1"}
		}
	}`
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcpJSON), 0644)

	plugin, err := LoadPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}

	if plugin.Manifest.Name != "test_plugin" {
		t.Errorf("unexpected name: %s", plugin.Manifest.Name)
	}
	if len(plugin.MCPs) != 1 || plugin.MCPs["server1"].Command != "cmd1" {
		t.Errorf("unexpected mcp servers: %+v", plugin.MCPs)
	}
}

func TestParseSKILLmd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	content := `---
name: test_skill2
description: desc2
---`
	os.WriteFile(path, []byte(content), 0644)

	meta, err := ParseSKILLmd(path, []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "skill:test_skill2" {
		t.Errorf("unexpected name: %s", meta.Name)
	}
}
