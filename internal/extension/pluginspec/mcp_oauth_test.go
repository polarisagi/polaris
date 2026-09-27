package pluginspec

import (
	"path/filepath"
	"testing"
)

func findMCPServer(t *testing.T, servers []MCPServer, name string) MCPServer {
	t.Helper()
	for _, s := range servers {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("server %q not found in %+v", name, servers)
	return MCPServer{}
}

func hasDiagRule(diags []Diagnostic, rule string) bool {
	for _, d := range diags {
		if d.Rule == rule {
			return true
		}
	}
	return false
}

// TestMCPOAuth_ClaudeOAuthObject Claude .mcp.json 的 oauth.{clientId,authServerMetadataUrl,scopes}
// 归一化为 MCPOAuth；callbackPort 记诊断（不适用，Polaris 回调走网关）不落入归一化结构。
func TestMCPOAuth_ClaudeOAuthObject(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"remote":{
		"type":"http","url":"https://mcp.example.com/mcp",
		"oauth":{"clientId":"claude-client-1","callbackPort":51234,"authServerMetadataUrl":"https://as.example.com/.well-known/oauth-authorization-server","scopes":["files:read","files:write"]}
	}}}`)

	servers, diags := ListMCPServersInDir(dir)
	srv := findMCPServer(t, servers, "remote")
	if srv.OAuthConfig == nil {
		t.Fatal("expected non-nil OAuthConfig")
	}
	if srv.OAuthConfig.ClientID != "claude-client-1" || srv.OAuthConfig.AuthServerMetadataURL != "https://as.example.com/.well-known/oauth-authorization-server" {
		t.Errorf("unexpected OAuthConfig: %+v", srv.OAuthConfig)
	}
	if len(srv.OAuthConfig.Scopes) != 2 {
		t.Errorf("unexpected scopes: %v", srv.OAuthConfig.Scopes)
	}
	if !hasDiagRule(diags, RuleMCPOAuthCallbackPort) {
		t.Errorf("expected %s diagnostic for callbackPort, got %+v", RuleMCPOAuthCallbackPort, diags)
	}
}

// TestMCPOAuth_CodexTopLevelScopes Codex 的顶层 scopes 字段（非嵌套 oauth 对象）落入 Extra，
// 归一化时并入 MCPOAuth.Scopes。
func TestMCPOAuth_CodexTopLevelScopes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"remote":{
		"type":"http","url":"https://mcp.example.com/mcp","scopes":["repo:read"]
	}}}`)

	servers, _ := ListMCPServersInDir(dir)
	srv := findMCPServer(t, servers, "remote")
	if srv.OAuthConfig == nil || len(srv.OAuthConfig.Scopes) != 1 || srv.OAuthConfig.Scopes[0] != "repo:read" {
		t.Fatalf("expected Codex top-level scopes normalized, got %+v", srv.OAuthConfig)
	}
}

// TestMCPOAuth_MergesOAuthObjectAndCodexScopes oauth 对象与顶层 scopes 同时出现时取并集去重。
func TestMCPOAuth_MergesOAuthObjectAndCodexScopes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"remote":{
		"type":"http","url":"https://mcp.example.com/mcp",
		"oauth":{"clientId":"c1","scopes":["a","b"]},
		"scopes":["b","c"]
	}}}`)

	servers, _ := ListMCPServersInDir(dir)
	srv := findMCPServer(t, servers, "remote")
	if srv.OAuthConfig == nil {
		t.Fatal("expected non-nil OAuthConfig")
	}
	want := map[string]bool{"a": true, "b": true, "c": true}
	if len(srv.OAuthConfig.Scopes) != len(want) {
		t.Fatalf("expected union of 3 scopes, got %v", srv.OAuthConfig.Scopes)
	}
	for _, s := range srv.OAuthConfig.Scopes {
		if !want[s] {
			t.Errorf("unexpected scope %q", s)
		}
	}
}

// TestMCPOAuth_StdioIgnoresOAuth STDIO 传输 SHOULD NOT 走 basic_authorization.md：
// 声明的 oauth 一律忽略（OAuthConfig 为 nil）并记诊断，不当作错误阻断解析。
func TestMCPOAuth_StdioIgnoresOAuth(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"local":{
		"type":"stdio","command":"node","args":["server.js"],
		"oauth":{"clientId":"should-be-ignored"}
	}}}`)

	servers, diags := ListMCPServersInDir(dir)
	srv := findMCPServer(t, servers, "local")
	if srv.OAuthConfig != nil {
		t.Errorf("expected nil OAuthConfig for stdio server, got %+v", srv.OAuthConfig)
	}
	if !hasDiagRule(diags, RuleMCPOAuthStdio) {
		t.Errorf("expected %s diagnostic, got %+v", RuleMCPOAuthStdio, diags)
	}
}

// TestMCPOAuth_NoDeclarationLeavesNilConfig 未声明 oauth/scopes 的普通服务器不产生
// OAuthConfig 也不产生任何 oauth 相关诊断。
func TestMCPOAuth_NoDeclarationLeavesNilConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"plain":{
		"type":"http","url":"https://mcp.example.com/mcp"
	}}}`)

	servers, diags := ListMCPServersInDir(dir)
	srv := findMCPServer(t, servers, "plain")
	if srv.OAuthConfig != nil {
		t.Errorf("expected nil OAuthConfig, got %+v", srv.OAuthConfig)
	}
	if hasDiagRule(diags, RuleMCPOAuthCallbackPort) || hasDiagRule(diags, RuleMCPOAuthStdio) {
		t.Errorf("unexpected oauth diagnostics: %+v", diags)
	}
}
