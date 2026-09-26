package pluginspec

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestLoadMarketplace_ClaudeAndCodexMerged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), `{
	  "name": "team", "owner": {"name": "Team"}, "metadata": {"pluginRoot": "./plugins", "description": "d"},
	  "allowCrossMarketplaceDependenciesOn": ["shared"],
	  "plugins": [
	    {"name": "fmt", "source": "./plugins/fmt", "strict": false, "dependencies": ["lint", {"name":"vault","version":"~2.1.0"}]},
	    {"name": "bare", "source": "bare"},
	    {"name": "gh", "source": {"source": "github", "repo": "org/gh", "ref": "v1", "sha": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"}},
	    {"name": "sub", "source": {"source": "git-subdir", "url": "org/mono", "path": "./tools/sub"}},
	    {"name": "pkg", "source": {"source": "npm", "package": "@org/pkg", "version": "^2.0.0"}},
	    {"name": "zip", "source": {"source": "archive", "url": "https://x.example/p.zip", "sha256": "6bfa50e3d2e00c052b46abe51fff89346ac803e45771f76dcf6df1ab74cca5e1"}},
	    {"name": "cmd", "source": {"source": "command", "command": "my-tool path"}},
	    {"name": "esc", "source": "./../outside"},
	    {"name": "fmt", "source": "./dup"},
	    {"name": "badsha", "source": {"source": "url", "url": "https://g.example/r.git", "sha": "abc"}}
	  ]}`)
	writeFile(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), `{
	  "name": "ignored-second-name", "interface": {"displayName": "Team Plugins"},
	  "plugins": [
	    {"name": "fmt", "source": {"source": "local", "path": "./plugins/fmt"}, "policy": {"installation": "INSTALLED_BY_DEFAULT", "authentication": "ON_USE"}, "category": "Dev"},
	    {"name": "codex-only", "source": "./plugins/codex-only", "policy": {"installation": "AVAILABLE", "authentication": "ON_INSTALL"}}
	  ]}`)
	m, err := LoadMarketplace(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "team" || m.DisplayName != "Team Plugins" || m.PluginRoot != "./plugins" || len(m.AllowCrossMarketplaceDeps) != 1 {
		t.Fatalf("identity: %+v", m)
	}
	byName := map[string]MarketplaceEntry{}
	for _, e := range m.Plugins {
		byName[e.Name] = e
	}
	want := []string{"fmt", "bare", "gh", "sub", "pkg", "zip", "codex-only"}
	if len(m.Plugins) != len(want) {
		t.Fatalf("entries: %v diags: %v", names(m.Plugins), m.Diagnostics)
	}
	fmtEntry := byName["fmt"]
	if fmtEntry.Strict || fmtEntry.Source.Path != filepath.Join(root, "plugins", "fmt") || fmtEntry.Policy == nil ||
		fmtEntry.Policy.Installation != "INSTALLED_BY_DEFAULT" || fmtEntry.Category != "Dev" || len(fmtEntry.Dependencies) != 2 {
		t.Fatalf("fmt: %+v", fmtEntry)
	}
	if byName["bare"].Source.Path != filepath.Join(root, "plugins", "bare") || byName["sub"].Source.Path != "tools/sub" ||
		byName["gh"].Source.Type != SourceGitHub || byName["pkg"].Source.Package != "@org/pkg" || !byName["zip"].Strict {
		t.Fatalf("sources: %+v", byName)
	}
	diag := joinDiags(m.Diagnostics)
	for _, want := range []string{"command sources are not supported", `Path contains ".."`, `Duplicate plugin name "fmt"`, "full 40-character"} {
		if !strings.Contains(diag, want) {
			t.Errorf("missing diagnostic %q in %s", want, diag)
		}
	}
}

func TestLoadMarketplace_Missing(t *testing.T) {
	if _, err := LoadMarketplace(t.TempDir()); !errors.Is(err, ErrNotAMarketplace) {
		t.Fatalf("got %v", err)
	}
}

func TestReservedMarketplaceName(t *testing.T) {
	for name, want := range map[string]bool{"claude-plugins-official": true, "claude.code.plugins": true, "npm": true,
		"claudeai-x": true, "my-team": false} {
		if got := ReservedMarketplaceName(name, false); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if ReservedMarketplaceName("claude-plugins-official", true) {
		t.Error("official names are allowed from github.com/anthropics/")
	}
}

func TestLoad_MarketplaceEntryOverlay(t *testing.T) {
	// 无清单：条目即清单。
	bare := t.TempDir()
	writeFile(t, filepath.Join(bare, "skills", "a", "SKILL.md"), "---\nname: a\ndescription: d\n---\nx")
	writeFile(t, filepath.Join(bare, "extra", "b", "SKILL.md"), "---\nname: b\ndescription: d\n---\nx")
	entry := &MarketplaceEntry{Name: "bare", Strict: false, Manifest: []byte(`{"name":"bare","skills":["./extra"],"defaultEnabled":false}`)}
	p, err := Load(bare, LoadOptions{Entry: entry})
	if err != nil || p.Name != "bare" || len(p.Skills) != 2 || p.DefaultEnabled {
		t.Fatalf("entry as manifest: %+v %v", p, err)
	}

	// 有清单 + strict:false + 条目组件字段 → 冲突。
	withManifest := t.TempDir()
	writeFile(t, filepath.Join(withManifest, ".claude-plugin", "plugin.json"), `{"name":"m","description":"from manifest",
	  "hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"a"}]}],"Stop":[{"hooks":[{"type":"command","command":"b"}]}]}}`)
	conflict := &MarketplaceEntry{Name: "m", Strict: false, Manifest: []byte(`{"name":"m","skills":["./x"]}`)}
	if _, err := Load(withManifest, LoadOptions{Entry: conflict}); err == nil || !apperr.IsCode(err, apperr.CodeInvalidInput) ||
		!strings.Contains(err.Error(), "conflicting manifests") {
		t.Fatalf("strict:false conflict: %v", err)
	}

	// 有清单 + strict：条目 hooks 按事件替换；展示字段以条目为准；条目 mcpServers 不生效。
	strict := &MarketplaceEntry{Name: "m", Strict: true, Description: "from entry", Manifest: []byte(`{"name":"m",
	  "hooks":{"Stop":[{"hooks":[{"type":"command","command":"entry"}]}]},"mcpServers":{"x":{"command":"y"}}}`)}
	p, err = Load(withManifest, LoadOptions{Entry: strict})
	if err != nil || p.Description != "from entry" || len(p.MCPServers) != 0 || len(p.Hooks) != 2 {
		t.Fatalf("strict overlay: %+v %v", p, err)
	}
	for _, h := range p.Hooks {
		if h.Source != entryDocPath+"#hooks" && h.Events["Stop"] != nil {
			t.Fatal("entry Stop hooks must replace the manifest's Stop event")
		}
	}
}

func names(es []MarketplaceEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func joinDiags(ds []Diagnostic) string {
	var sb strings.Builder
	for _, d := range ds {
		sb.WriteString(d.String() + "\n")
	}
	return sb.String()
}
