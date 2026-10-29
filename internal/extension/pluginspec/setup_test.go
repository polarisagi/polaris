package pluginspec

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func loadManifest(t *testing.T, manifest string) *Plugin {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p, err := Load(dir, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

func TestSetupTools_Valid(t *testing.T) {
	p := loadManifest(t, `{"name":"bridge","version":"1.0.0","description":"d",
		"extensions":{"ai.polarisagi":{"setupTool":"bridge_setup","statusTool":"bridge.status"}}}`)
	if p.SetupTool != "bridge_setup" || p.StatusTool != "bridge.status" {
		t.Fatalf("got setup=%q status=%q", p.SetupTool, p.StatusTool)
	}
}

func TestSetupTools_StatusWithoutSetupIgnored(t *testing.T) {
	p := loadManifest(t, `{"name":"bridge","version":"1.0.0","description":"d",
		"extensions":{"ai.polarisagi":{"statusTool":"bridge_status"}}}`)
	if p.SetupTool != "" || p.StatusTool != "" {
		t.Fatalf("statusTool must be dropped without setupTool, got setup=%q status=%q", p.SetupTool, p.StatusTool)
	}
	if !hasRule(p.Diagnostics, RuleSetupStatusOrphan, SeverityWarning) {
		t.Fatalf("missing diagnostic %s: %v", RuleSetupStatusOrphan, p.Diagnostics)
	}
}

func TestSetupTools_InvalidNames(t *testing.T) {
	for name, body := range map[string]string{
		"empty":      `{"setupTool":""}`,
		"non-string": `{"setupTool":42}`,
		"bad chars":  `{"setupTool":"a b/c"}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := loadManifest(t, `{"name":"bridge","version":"1.0.0","description":"d","extensions":{"ai.polarisagi":`+body+`}}`)
			if p.SetupTool != "" {
				t.Fatalf("setupTool must be ignored, got %q", p.SetupTool)
			}
		})
	}
	p := loadManifest(t, `{"name":"bridge","version":"1.0.0","description":"d","extensions":{"ai.polarisagi":{"setupTool":"x y"}}}`)
	if !hasRule(p.Diagnostics, RuleSetupToolShape, SeverityWarning) {
		t.Fatalf("missing diagnostic %s: %v", RuleSetupToolShape, p.Diagnostics)
	}
}

func TestDefaultPrompts(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string
		diag      bool
	}{
		{"string", `"hello"`, []string{"hello"}, false},
		{"array", `["a","b"]`, []string{"a", "b"}, false},
		{"over limit truncated", `["a","b","c","d"]`, []string{"a", "b", "c"}, true},
		{"blank dropped", `["", "  ", "x"]`, []string{"x"}, false},
		{"all blank", `["", " "]`, nil, false},
		{"illegal type", `{"a":1}`, nil, true},
		{"number", `7`, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := loadManifest(t, `{"name":"dp","version":"1.0.0","description":"d","interface":{"defaultPrompt":`+c.raw+`}}`)
			if !slices.Equal(p.DefaultPrompts, c.want) {
				t.Fatalf("got %v want %v", p.DefaultPrompts, c.want)
			}
			if hasRule(p.Diagnostics, RuleDefaultPromptShape, SeverityWarning) != c.diag {
				t.Fatalf("diagnostic presence = %v, want %v: %v", !c.diag, c.diag, p.Diagnostics)
			}
		})
	}
}
