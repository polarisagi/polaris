package pluginspec

import (
	"path/filepath"
	"testing"
)

func TestLoad_PolarisNamespaceComponents(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name":"official-plugin","version":"1.2.3","description":"root identity",
  "author":{"name":"Root Author"},
  "extensions":{"ai.polarisagi":{
    "name":"spoofed-name","version":"9.9.9","description":"spoofed description",
    "author":{"name":"Spoofed Author"},
    "displayName":"Namespace Display",
    "interface":{"displayName":"Private UI","category":"Productivity","defaultPrompt":["try it"]},
    "dependencies":[{"name":"polaris-browser-bridge","version":"^1.0.0"}],
    "skills":"./skills/","agents":["./agents/lead.md"],
    "channels":[{"server":"remote","displayName":"Remote channel","userConfig":{"token":{"type":"string","title":"Token","description":"Token","sensitive":true}}}],
    "userConfig":{"region":{"type":"string","title":"Region","description":"Region"}}
  }}
}`)
	writeFile(t, filepath.Join(root, "mcp.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"remote":{"type":"streamable-http","url":"https://example.com/mcp"}}}`)
	writeFile(t, filepath.Join(root, "skills", "lookup", "SKILL.md"), "---\nname: lookup\ndescription: Lookup\n---\n")
	writeFile(t, filepath.Join(root, "agents", "lead.md"), "---\nname: lead\ndescription: Lead\n---\n")

	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "official-plugin" || p.Version != "1.2.3" || p.Description != "root identity" || p.Author.Name != "Root Author" {
		t.Fatalf("namespace must not override root identity: %+v", p)
	}
	if len(p.Formats) != 1 || p.Formats[0] != FormatAgentPlugins {
		t.Fatalf("Formats must list only physical manifest formats: %v", p.Formats)
	}
	if p.DisplayName != "Namespace Display" || p.Interface == nil || p.Interface.DisplayName != "Private UI" || p.Interface.Category != "Productivity" || len(p.DefaultPrompts) != 1 || p.DefaultPrompts[0] != "try it" {
		t.Fatalf("interface/defaultPrompt: interface=%+v prompts=%v", p.Interface, p.DefaultPrompts)
	}
	if len(p.Dependencies) != 1 || p.Dependencies[0].Name != "polaris-browser-bridge" || p.Dependencies[0].Version != "^1.0.0" {
		t.Fatalf("dependencies: %+v", p.Dependencies)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "lookup" || len(p.Agents) != 1 || p.Agents[0].Name != "lead" {
		t.Fatalf("skills/agents whitelist: skills=%+v agents=%+v", p.Skills, p.Agents)
	}
	if len(p.MCPServers) != 1 || p.MCPServers[0].Type != MCPTypeHTTP {
		t.Fatalf("portable MCP type normalization: %+v", p.MCPServers)
	}
	if len(p.UserConfig) != 1 || p.UserConfig[0].Key != "region" || len(p.Channels) != 1 || p.Channels[0].Server != "remote" || len(p.Channels[0].UserConfig) != 1 {
		t.Fatalf("channels/userConfig: channels=%+v config=%+v", p.Channels, p.UserConfig)
	}
}

func TestLoad_PolarisNamespaceKeepsExistingDependencyBoundaries(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"root","dependencies":["ignored-root"]}`)
	writeFile(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{"name":"root","dependencies":["ignored-codex"]}`)
	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Dependencies) != 0 {
		t.Fatalf("legacy root/Codex dependencies must remain ignored: %+v", p.Dependencies)
	}
}

func TestLoad_PortableMCPTypeMustBeExplicitAndStandard(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		schemaError bool
		typeError   bool
	}{
		{"valid streamable-http", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"remote":{"type":"streamable-http","url":"https://example.com/mcp"}}}`, false, false},
		{"legacy http spelling", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"remote":{"type":"http","url":"https://example.com/mcp"}}}`, false, true},
		{"missing type", `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"remote":{"url":"https://example.com/mcp"}}}`, false, true},
		{"wrong schema", `{"$schema":"https://example.org/mcp.json","mcpServers":{"remote":{"type":"stdio","command":"node"}}}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"portable"}`)
			writeFile(t, filepath.Join(root, "mcp.json"), tc.body)
			p, err := Load(root, LoadOptions{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if hasRule(p.Diagnostics, RuleMCPPortableSchema, SeverityError) != tc.schemaError {
				t.Fatalf("schema diagnostics: %+v", p.Diagnostics)
			}
			if hasRule(p.Diagnostics, RuleMCPPortableType, SeverityError) != tc.typeError {
				t.Fatalf("type diagnostics: %+v", p.Diagnostics)
			}
		})
	}
}

func TestLoad_PolarisMCPServerMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mcp     string
		wantURL string
	}{
		{
			name:    "namespace oauth metadata",
			mcp:     `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"box":{"type":"streamable-http","url":"https://mcp.box.com"}}}`,
			wantURL: "https://account.box.com/.well-known/oauth-authorization-server",
		},
		{
			name:    "explicit file oauth wins",
			mcp:     `{"mcpServers":{"box":{"type":"streamable-http","url":"https://mcp.box.com","oauth":{"authServerMetadataUrl":"https://explicit.example/oauth"}}}}`,
			wantURL: "https://explicit.example/oauth",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"box","extensions":{"ai.polarisagi":{"mcpServerMetadata":{"box":{"oauth":{"authServerMetadataUrl":"https://account.box.com/.well-known/oauth-authorization-server"}}}}}}`)
			writeFile(t, filepath.Join(root, "mcp.json"), tc.mcp)
			p, err := Load(root, LoadOptions{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(p.MCPServers) != 1 || p.MCPServers[0].OAuthConfig == nil || p.MCPServers[0].OAuthConfig.AuthServerMetadataURL != tc.wantURL {
				t.Fatalf("MCP metadata was not applied: %+v", p.MCPServers)
			}
		})
	}
}

func TestLoad_InvalidExplicitOAuthDoesNotFallBackToNamespace(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.json"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"box","extensions":{"ai.polarisagi":{"mcpServerMetadata":{"box":{"oauth":{"authServerMetadataUrl":"https://namespace.example/oauth"}}}}}}`)
	writeFile(t, filepath.Join(root, "mcp.json"), `{"mcpServers":{"box":{"type":"streamable-http","url":"https://mcp.box.com","oauth":"invalid"}}}`)
	p, err := Load(root, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.MCPServers) != 1 || p.MCPServers[0].OAuthConfig != nil || string(p.MCPServers[0].OAuth) != `"invalid"` {
		t.Fatalf("invalid explicit OAuth must retain its source without namespace fallback: %+v", p.MCPServers)
	}
	if !hasRule(p.Diagnostics, RuleMCPParse, SeverityWarning) {
		t.Fatalf("invalid explicit OAuth diagnostic missing: %+v", p.Diagnostics)
	}
}
