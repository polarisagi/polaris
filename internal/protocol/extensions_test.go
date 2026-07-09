package protocol

import (
	"encoding/json"
	"testing"
)

func TestPluginBundleManifest_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		jsonStr  string
		expected PluginBundleManifest
	}{
		{
			name: "string skills and hooks",
			jsonStr: `{
				"name": "test1",
				"skills": "./skills/",
				"hooks": "./hooks.json"
			}`,
			expected: PluginBundleManifest{
				Name:      "test1",
				SkillsDir: "./skills/",
				HooksFile: "./hooks.json",
			},
		},
		{
			name: "array skills and map hooks",
			jsonStr: `{
				"name": "test2",
				"skills": [{"path": "./skill1"}],
				"hooks": {"install": "./install.sh"}
			}`,
			expected: PluginBundleManifest{
				Name: "test2",
				Skills: []BundleSkillRef{
					{Path: "./skill1"},
				},
				Hooks: map[string]string{
					"install": "./install.sh",
				},
			},
		},
		{
			name: "snake case mcp_servers and mcp_inline",
			jsonStr: `{
				"name": "test3",
				"mcp_servers": "./mcp.json",
				"mcp_inline": {
					"server1": {"command": "echo"}
				}
			}`,
			expected: PluginBundleManifest{
				Name:    "test3",
				MCPFile: "./mcp.json",
				MCPInline: map[string]MCPServerDef{
					"server1": {Command: "echo"},
				},
			},
		},
		{
			name: "camel case mcpServers and mcpInline",
			jsonStr: `{
				"name": "test4",
				"mcpServers": "./mcp.json",
				"mcpInline": {
					"server1": {"command": "echo"}
				}
			}`,
			expected: PluginBundleManifest{
				Name:    "test4",
				MCPFile: "./mcp.json",
				MCPInline: map[string]MCPServerDef{
					"server1": {Command: "echo"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var actual PluginBundleManifest
			err := json.Unmarshal([]byte(tc.jsonStr), &actual)
			if err != nil {
				t.Fatalf("Failed to unmarshal JSON: %v", err)
			}

			if actual.Name != tc.expected.Name {
				t.Errorf("Expected Name %q, got %q", tc.expected.Name, actual.Name)
			}
			if actual.SkillsDir != tc.expected.SkillsDir {
				t.Errorf("Expected SkillsDir %q, got %q", tc.expected.SkillsDir, actual.SkillsDir)
			}
			if actual.HooksFile != tc.expected.HooksFile {
				t.Errorf("Expected HooksFile %q, got %q", tc.expected.HooksFile, actual.HooksFile)
			}
			if len(actual.Skills) != len(tc.expected.Skills) {
				t.Errorf("Expected %d Skills, got %d", len(tc.expected.Skills), len(actual.Skills))
			} else if len(actual.Skills) > 0 && actual.Skills[0].Path != tc.expected.Skills[0].Path {
				t.Errorf("Expected Skill Path %q, got %q", tc.expected.Skills[0].Path, actual.Skills[0].Path)
			}
			if len(actual.Hooks) != len(tc.expected.Hooks) {
				t.Errorf("Expected %d Hooks, got %d", len(tc.expected.Hooks), len(actual.Hooks))
			} else if len(actual.Hooks) > 0 && actual.Hooks["install"] != tc.expected.Hooks["install"] {
				t.Errorf("Expected Hook install %q, got %q", tc.expected.Hooks["install"], actual.Hooks["install"])
			}
			if actual.MCPFile != tc.expected.MCPFile {
				t.Errorf("Expected MCPFile %q, got %q", tc.expected.MCPFile, actual.MCPFile)
			}
			if len(actual.MCPInline) != len(tc.expected.MCPInline) {
				t.Errorf("Expected %d MCPInline, got %d", len(tc.expected.MCPInline), len(actual.MCPInline))
			} else if len(actual.MCPInline) > 0 && actual.MCPInline["server1"].Command != tc.expected.MCPInline["server1"].Command {
				t.Errorf("Expected MCPInline server1 command %q, got %q", tc.expected.MCPInline["server1"].Command, actual.MCPInline["server1"].Command)
			}
		})
	}
}
