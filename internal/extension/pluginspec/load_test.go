package pluginspec

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func skillNames(p *Plugin) map[string]SkillKind {
	out := map[string]SkillKind{}
	for _, s := range p.Skills {
		out[s.Name] = s.Kind
	}
	return out
}

func TestLoad_ClaudePluginFullLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deploy-tools")
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{
  "name": "deploy-tools", "displayName": "Deploy Tools", "version": "1.2.0",
  "description": "Deployment", "author": {"name": "Example"},
  "defaultEnabled": false, "dependencies": ["secrets-vault", {"name": "x", "marketplace": "m"}],
  "userConfig": {
    "api_endpoint": {"type": "string", "title": "API endpoint", "description": "Endpoint"},
    "api_token": {"type": "string", "title": "Token", "description": "Token", "sensitive": true}
  },
  "mcpServers": {"deploy-api": {"command": "node", "args": ["${CLAUDE_PLUGIN_ROOT}/server.js"], "env": {"TOKEN": "${user_config.api_token}"}}},
  "channels": [{"server": "deploy-api", "displayName": "Deploy"}],
  "lspServers": {"go": {"command": "gopls", "extensionToLanguage": {".go": "go"}}},
  "futureField": true
}`)
	writeFile(t, filepath.Join(root, "skills", "deploy", "SKILL.md"), "---\nname: deploy\ndescription: Deploy it\n---\nbody")
	writeFile(t, filepath.Join(root, "commands", "status.md"), "---\ndescription: Show status\n---\nstatus")
	writeFile(t, filepath.Join(root, "commands", "frontend", "component.md"), "make component")
	writeFile(t, filepath.Join(root, "agents", "reviewer.md"), "---\nname: reviewer\ndescription: Reviews\ntools: Read, Grep\n---\nYou review.")
	writeFile(t, filepath.Join(root, "hooks", "hooks.json"), `{"hooks":{"PostToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"x"}]}]}}`)
	writeFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"remote":{"type":"http","url":"https://example.com/mcp"}}}`)
	writeFile(t, filepath.Join(root, "bin", "tool"), "#!/bin/sh")

	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "deploy-tools" || p.DisplayName != "Deploy Tools" || p.DefaultEnabled || p.Author.Name != "Example" {
		t.Fatalf("identity: %+v", p)
	}
	names := skillNames(p)
	if names["deploy"] != SkillKindSkill || names["status"] != SkillKindCommand || names["frontend:component"] != SkillKindCommand {
		t.Fatalf("skills/commands: %v", names)
	}
	if len(p.Agents) != 1 || p.Agents[0].Name != "reviewer" || len(p.Agents[0].Tools) != 2 {
		t.Fatalf("agents: %+v", p.Agents)
	}
	if len(p.Hooks) != 1 || p.Hooks[0].Digest == "" {
		t.Fatalf("hooks: %+v", p.Hooks)
	}
	if len(p.MCPServers) != 2 {
		t.Fatalf("mcp servers: %+v", p.MCPServers)
	}
	if len(p.UserConfig) != 2 || p.UserConfig[0].Key != "api_endpoint" || !p.UserConfig[1].Sensitive {
		t.Fatalf("userConfig order/fields: %+v", p.UserConfig)
	}
	if len(p.Channels) != 1 || len(p.Dependencies) != 2 || p.Dependencies[1].Marketplace != "m" {
		t.Fatalf("channels=%+v deps=%+v", p.Channels, p.Dependencies)
	}
	kinds := map[string]bool{}
	for _, u := range p.Unsupported {
		kinds[u.Kind] = true
	}
	if !kinds["lspServers"] || !kinds["bin"] {
		t.Fatalf("unsupported components not reported: %+v", p.Unsupported)
	}
	if !hasRule(p.Diagnostics, RuleManifestUnknown, SeverityWarning) {
		t.Fatalf("unknown top-level field must warn: %v", p.Diagnostics)
	}
}

func TestLoad_CodexPluginWithApps(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sales")
	writeFile(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{
  "name": "sales", "version": "1.0.0", "description": "d", "author": {"name": "OpenAI"},
  "skills": "./skills/", "mcpServers": "./.mcp.json", "apps": "./.app.json", "hooks": "./hooks/hooks.json",
  "interface": {"displayName": "Sales", "category": "Productivity", "defaultPrompt": ["a", "b"]}
}`)
	writeFile(t, filepath.Join(root, "skills", "pipeline", "SKILL.md"), "---\nname: pipeline\ndescription: d\n---\n")
	writeFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"crm":{"type":"streamable-http","url":"https://crm.example.com/mcp"}}}`)
	writeFile(t, filepath.Join(root, ".app.json"), `{"apps":{"salesforce":{"id":"connector_abc"},"slack":{"id":"plugin_asdk_app_1"}}}`)
	writeFile(t, filepath.Join(root, "hooks", "hooks.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"x"}]}]}}`)

	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.DisplayName != "Sales" || p.Interface.Category != "Productivity" {
		t.Fatalf("interface: %+v", p.Interface)
	}
	if len(p.Skills) != 1 || len(p.Hooks) != 1 {
		t.Fatalf("skills must dedupe default+declared dir, hooks must dedupe default+declared file: skills=%d hooks=%d", len(p.Skills), len(p.Hooks))
	}
	if len(p.MCPServers) != 1 || p.MCPServers[0].Type != MCPTypeHTTP {
		t.Fatalf("mcp: %+v", p.MCPServers)
	}
	if len(p.Apps) != 2 || p.Apps[0].Alias != "salesforce" || p.Apps[0].ConnectorID != "connector_abc" {
		t.Fatalf("apps: %+v", p.Apps)
	}
}

// agent-plugins 根清单的 extensions["com.openai"] 整体替换 .codex-plugin/plugin.json。
func TestLoad_AgentPluginsOverlayReplacesCodexManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "portable")
	writeFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "portable", "version": "1.0.0", "description": "d",
  "extensions": {"com.openai": {"apps": "./.app.json", "interface": {"displayName": "From Overlay"}}}
}`)
	writeFile(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{"name":"portable","interface":{"displayName":"From Codex File"}}`)
	writeFile(t, filepath.Join(root, "mcp.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"s":{"type":"stdio","command":"./bin/server"}}}`)
	writeFile(t, filepath.Join(root, ".app.json"), `{"apps":{"gh":{"id":"github"}}}`)
	writeFile(t, filepath.Join(root, "skills", "a", "SKILL.md"), "---\nname: a\ndescription: d\n---\n")

	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Interface == nil || p.Interface.DisplayName != "From Overlay" {
		t.Fatalf("overlay must replace codex manifest: %+v", p.Interface)
	}
	if !hasRule(p.Diagnostics, RuleManifestOverlay, SeverityWarning) {
		t.Fatalf("ignored codex file must be reported: %v", p.Diagnostics)
	}
	if len(p.MCPServers) != 1 || len(p.Apps) != 1 || len(p.Skills) != 1 {
		t.Fatalf("components: mcp=%d apps=%d skills=%d", len(p.MCPServers), len(p.Apps), len(p.Skills))
	}
}

func TestLoad_NoManifestUsesDefaultsAndFallbackName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dir-name")
	writeFile(t, filepath.Join(root, "skills", "x", "SKILL.md"), "---\nname: x\ndescription: d\n---\n")
	p, err := Load(root, LoadOptions{FallbackName: "market-entry"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "market-entry" || len(p.Formats) != 0 || len(p.Skills) != 1 {
		t.Fatalf("got %+v", p)
	}
}

func TestLoad_RootSkillSinglePlugin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "solo")
	writeFile(t, filepath.Join(root, "SKILL.md"), "---\ndescription: Solo skill\n---\n")
	p, err := Load(root, LoadOptions{})
	if err != nil || len(p.Skills) != 1 || p.Skills[0].Name != "solo" {
		t.Fatalf("err=%v plugin=%+v", err, p)
	}
	if hasRule(p.Diagnostics, RuleSkillNameDir, SeverityWarning) {
		t.Fatalf("root skill must not get name/dir warning")
	}
}

func TestLoad_NotAPlugin(t *testing.T) {
	_, err := Load(t.TempDir(), LoadOptions{})
	if !errors.Is(err, ErrNotAPlugin) {
		t.Fatalf("expected ErrNotAPlugin, got %v", err)
	}
}

func TestLoad_InvalidPluginName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"bad name@x"}`)
	if _, err := Load(root, LoadOptions{}); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("expected ErrInvalidPlugin, got %v", err)
	}
}

// 路径越界 / 缺 "./" 前缀 / 不存在：组件单独失败，其余组件照常加载。
func TestLoad_PathRulesIsolateComponent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	outside := filepath.Join(filepath.Dir(root), "outside")
	writeFile(t, filepath.Join(outside, "SKILL.md"), "---\nname: outside\ndescription: d\n---\n")
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{
  "name": "p", "skills": ["../outside", "skills-no-prefix", "./missing"],
  "agents": ["./agents"], "hooks": "./nope.json"
}`)
	writeFile(t, filepath.Join(root, "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: d\n---\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{
  "name": "p", "skills": ["./../outside", "skills-no-prefix", "./missing", "./linked"],
  "hooks": "./nope.json"
}`)
	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "ok" {
		t.Fatalf("only in-root skill must load: %+v", skillNames(p))
	}
	for _, rule := range []string{RulePathContainment, RulePathPrefix, RulePathExists} {
		if !hasRule(p.Diagnostics, rule, SeverityError) {
			t.Errorf("expected %s error, got %v", rule, p.Diagnostics)
		}
	}
}

func TestLoad_UserConfigStrictAndOptions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{
  "name": "p",
  "userConfig": {
    "good": {"type": "string", "title": "T", "description": "D", "options": ["a","b"], "default": "a"},
    "unknown_key": {"type": "string", "title": "T", "description": "D", "bogus": 1},
    "bad_type": {"type": "list", "title": "T", "description": "D"},
    "1starts_digit": {"type": "string", "title": "T", "description": "D"},
    "sens_opts": {"type": "string", "title": "T", "description": "D", "sensitive": true, "options": ["a"]}
  },
  "mcpServers": {"s": {"command": "x"}},
  "channels": [{"server": "missing"}, {"server": "s", "extra": 1}]
}`)
	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.UserConfig) != 1 || p.UserConfig[0].Key != "good" {
		t.Fatalf("only the valid option survives: %+v", p.UserConfig)
	}
	if len(p.Channels) != 0 {
		t.Fatalf("invalid channels must be dropped: %+v", p.Channels)
	}
}

func TestLoad_MCPBBundle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"p","mcpServers":"./server.mcpb"}`)
	bundle := filepath.Join(root, "server.mcpb")
	writeZip(t, bundle, map[string]string{
		"manifest.json": `{"name":"weather","version":"1.0.0","server":{"type":"node","entry_point":"server/index.js",
  "mcp_config":{"command":"node","args":["${__dirname}/server/index.js"],"env":{"K":"${user_config.api_key}"}}},
  "user_config":{"api_key":{"type":"string","title":"API key","description":"Key","sensitive":true}}}`,
		"server/index.js": "console.log(1)",
	})
	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.MCPServers) != 1 || p.MCPServers[0].Name != "weather" {
		t.Fatalf("bundle server: %+v diags=%v", p.MCPServers, p.Diagnostics)
	}
	want := filepath.ToSlash(filepath.Join(root, ".mcpb-cache", "server")) + "/server/index.js"
	if p.MCPServers[0].Args[0] != want {
		t.Fatalf("__dirname not expanded: %q want %q", p.MCPServers[0].Args[0], want)
	}
	if len(p.UserConfig) != 1 || p.UserConfig[0].Key != "api_key" {
		t.Fatalf("bundle user_config not merged: %+v", p.UserConfig)
	}
}

func TestLoad_MCPBRejectsZipSlip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"p","mcpServers":"./evil.mcpb"}`)
	writeZip(t, filepath.Join(root, "evil.mcpb"), map[string]string{"../../escape.txt": "x", "manifest.json": "{}"})
	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.MCPServers) != 0 || !hasRule(p.Diagnostics, RuleMCPBundle, SeverityError) {
		t.Fatalf("zip slip must be rejected: servers=%v diags=%v", p.MCPServers, p.Diagnostics)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); statErr == nil {
		t.Fatalf("file escaped the cache directory")
	}
}

func TestLoad_RemoteBundleRequiresInstallerFetch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	url := "https://example.com/server.mcpb"
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"p","mcpServers":["`+url+`"],"skills":"./s"}`)
	writeFile(t, filepath.Join(root, "s", "SKILL.md"), "---\nname: s\ndescription: d\n---\n")
	if got := ListRemoteBundles(root); len(got) != 1 || got[0] != url {
		t.Fatalf("ListRemoteBundles = %v", got)
	}
	p, _ := Load(root, LoadOptions{})
	if !hasRule(p.Diagnostics, RuleMCPBundleRemote, SeverityError) {
		t.Fatalf("unfetched remote bundle must error: %v", p.Diagnostics)
	}
	local := filepath.Join(t.TempDir(), "dl.mcpb")
	writeZip(t, local, map[string]string{"manifest.json": `{"name":"r","server":{"mcp_config":{"command":"node"}}}`})
	p, _ = Load(root, LoadOptions{RemoteBundles: map[string]string{url: local}})
	if len(p.MCPServers) != 1 || p.MCPServers[0].Name != "r" {
		t.Fatalf("fetched remote bundle must load: %+v %v", p.MCPServers, p.Diagnostics)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
