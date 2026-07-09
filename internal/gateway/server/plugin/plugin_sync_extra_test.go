package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestParseAIPluginEntry(t *testing.T) {
	manifest := `{
		"name_for_human": "Test Plugin",
		"description_for_human": "A test plugin",
		"api": {
			"url": "http://localhost:8080/openapi.yaml"
		}
	}`
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "ai-plugin.json"), []byte(manifest), 0644)

	mp := protocol.Marketplace{ID: "testmp"}
	entry, err := parseAIPluginEntry(filepath.Join(tmpDir, "ai-plugin.json"), tmpDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "Test Plugin" || entry.Description != "A test plugin" {
		t.Errorf("unexpected entry: %+v", entry)
	}
}

func TestParsePluginTOMLEntry(t *testing.T) {
	manifest := `
[plugin]
name = "TOML Plugin"
description = "TOML plugin desc"
`
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "plugin.toml"), []byte(manifest), 0644)

	mp := protocol.Marketplace{ID: "testmp"}
	entry, err := parsePluginTOMLEntry(filepath.Join(tmpDir, "plugin.toml"), tmpDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "TOML Plugin" || entry.Description != "TOML plugin desc" {
		t.Errorf("unexpected entry: %+v", entry)
	}
}

func TestParseGoogleSkillsEntry(t *testing.T) {
	manifest := `
skills:
  - name: "Google Skill"
    description: "Google skill desc"
`
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "skills.yaml"), []byte(manifest), 0644)

	mp := protocol.Marketplace{ID: "testmp"}
	entry, err := parseGoogleSkillsEntry(filepath.Join(tmpDir, "skills.yaml"), tmpDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entry) == 0 || entry[0].Name != "Google Skill" || entry[0].Description != "Google skill desc" {
		t.Errorf("unexpected entry: %+v", entry)
	}
}

func TestParsePackageJSONEntry(t *testing.T) {
	manifest := `{
		"name": "NPM Plugin mcp",
		"description": "NPM plugin desc",
		"dependencies": {
			"mcp": "1.0.0"
		}
	}`
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "package.json"), []byte(manifest), 0644)

	mp := protocol.Marketplace{ID: "testmp"}
	entry, err := parsePackageJSONEntry(filepath.Join(tmpDir, "package.json"), tmpDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "NPM Plugin mcp" || entry.Description != "NPM plugin desc" {
		t.Errorf("unexpected entry: %+v", entry)
	}
}

func TestParsePyProjectTOMLEntry(t *testing.T) {
	manifest := `
[project]
name = "Py Plugin mcp"
description = "Py plugin desc"
dependencies = ["mcp"]
`
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "pyproject.toml"), []byte(manifest), 0644)

	mp := protocol.Marketplace{ID: "testmp"}
	entry, err := parsePyProjectTOMLEntry(filepath.Join(tmpDir, "pyproject.toml"), tmpDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "Py Plugin mcp" || entry.Description != "Py plugin desc" {
		t.Errorf("unexpected entry: %+v", entry)
	}
}
