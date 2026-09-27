package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

// newMCPOAuthTestDB 直接用 SSoT DDL（015_mcp_servers.sql + 040_mcp_oauth.sql）建表：
// 列清单漂移时 INSERT/SELECT 失败即暴露，与 repo_llm_call_test.go 同一模式。
func newMCPOAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, f := range []string{"015_mcp_servers.sql", "040_mcp_oauth.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("exec %s: %v", f, err)
		}
	}
	return db
}

func TestSQLiteExtensionRepository_MCPServerOAuthColumnRoundTrip(t *testing.T) {
	db := newMCPOAuthTestDB(t)
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	row := types.MCPServerRow{ID: "mcp-1", Name: "srv", Transport: "streamable_http", URL: "https://mcp.example.com/mcp",
		Headers: "{}", OAuth: `{"client_id":"cid-1","scopes":["a","b"]}`, Enabled: true, Timeout: 30, TrustTier: 3,
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPServer(ctx, row); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}

	got, err := r.GetMCPServer(ctx, "mcp-1")
	if err != nil || got == nil {
		t.Fatalf("GetMCPServer: %v %v", got, err)
	}
	if got.OAuth != `{"client_id":"cid-1","scopes":["a","b"]}` {
		t.Errorf("unexpected OAuth column: %q", got.OAuth)
	}

	list, err := r.ListMCPServers(ctx)
	if err != nil || len(list) != 1 || list[0].OAuth == "" {
		t.Fatalf("ListMCPServers: %v %+v", err, list)
	}
}

func TestSQLiteExtensionRepository_MCPServerOAuthColumnDefaultsToEmptyObject(t *testing.T) {
	db := newMCPOAuthTestDB(t)
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	row := types.MCPServerRow{ID: "mcp-2", Name: "srv2", Transport: "stdio", Headers: "{}",
		Enabled: true, Timeout: 30, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPServer(ctx, row); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	got, err := r.GetMCPServer(ctx, "mcp-2")
	if err != nil || got == nil {
		t.Fatalf("GetMCPServer: %v %v", got, err)
	}
	if got.OAuth != "{}" {
		t.Errorf("expected default '{}', got %q", got.OAuth)
	}
}

func TestSQLiteExtensionRepository_MCPOAuthClientRoundTrip(t *testing.T) {
	db := newMCPOAuthTestDB(t)
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	if got, err := r.GetMCPOAuthClient(ctx, "https://as.example.com", "https://gw.example.com/oauth/mcp/callback"); err != nil || got != nil {
		t.Fatalf("expected nil for missing client, got %+v err=%v", got, err)
	}

	row := types.MCPOAuthClientRow{Issuer: "https://as.example.com", RedirectURI: "https://gw.example.com/oauth/mcp/callback",
		ClientID: "dcr-client-1", ClientSecretEnc: "v1:encrypted", RegistrationMethod: "dcr", CreatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPOAuthClient(ctx, row); err != nil {
		t.Fatalf("UpsertMCPOAuthClient: %v", err)
	}
	got, err := r.GetMCPOAuthClient(ctx, row.Issuer, row.RedirectURI)
	if err != nil || got == nil {
		t.Fatalf("GetMCPOAuthClient: %v %v", got, err)
	}
	if got.ClientID != "dcr-client-1" || got.ClientSecretEnc != "v1:encrypted" || got.RegistrationMethod != "dcr" {
		t.Errorf("unexpected row: %+v", got)
	}

	// 不同 issuer 或不同 redirect_uri 是独立键（授权服务器绑定隔离）。
	if got, err := r.GetMCPOAuthClient(ctx, "https://other-as.example.com", row.RedirectURI); err != nil || got != nil {
		t.Fatalf("expected nil for different issuer, got %+v err=%v", got, err)
	}

	// Upsert 更新 client_id（模拟重新注册）。
	row.ClientID = "dcr-client-2"
	if err := r.UpsertMCPOAuthClient(ctx, row); err != nil {
		t.Fatalf("UpsertMCPOAuthClient update: %v", err)
	}
	got2, err := r.GetMCPOAuthClient(ctx, row.Issuer, row.RedirectURI)
	if err != nil || got2 == nil || got2.ClientID != "dcr-client-2" {
		t.Fatalf("expected updated client_id, got %+v err=%v", got2, err)
	}
}

func TestSQLiteExtensionRepository_MCPOAuthTokenRoundTrip(t *testing.T) {
	db := newMCPOAuthTestDB(t)
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	if got, err := r.GetMCPOAuthToken(ctx, "srv-1"); err != nil || got != nil {
		t.Fatalf("expected nil for missing token, got %+v err=%v", got, err)
	}

	row := types.MCPOAuthTokenRow{ServerID: "srv-1", Issuer: "https://as.example.com", Resource: "https://mcp.example.com/mcp",
		AccessTokenEnc: "v1:access", RefreshTokenEnc: "v1:refresh", TokenType: "Bearer", Scopes: "a b",
		ExpiresAt: "2026-01-01T01:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPOAuthToken(ctx, row); err != nil {
		t.Fatalf("UpsertMCPOAuthToken: %v", err)
	}
	got, err := r.GetMCPOAuthToken(ctx, "srv-1")
	if err != nil || got == nil {
		t.Fatalf("GetMCPOAuthToken: %v %v", got, err)
	}
	if got.AccessTokenEnc != "v1:access" || got.Scopes != "a b" {
		t.Errorf("unexpected row: %+v", got)
	}

	// Upsert 覆盖（刷新场景）：同一 server_id 主键更新。
	row.AccessTokenEnc = "v1:access-refreshed"
	row.ExpiresAt = "2026-01-01T02:00:00Z"
	if err := r.UpsertMCPOAuthToken(ctx, row); err != nil {
		t.Fatalf("UpsertMCPOAuthToken refresh: %v", err)
	}
	got2, err := r.GetMCPOAuthToken(ctx, "srv-1")
	if err != nil || got2 == nil || got2.AccessTokenEnc != "v1:access-refreshed" {
		t.Fatalf("expected refreshed access token, got %+v err=%v", got2, err)
	}

	if err := r.DeleteMCPOAuthToken(ctx, "srv-1"); err != nil {
		t.Fatalf("DeleteMCPOAuthToken: %v", err)
	}
	if got, err := r.GetMCPOAuthToken(ctx, "srv-1"); err != nil || got != nil {
		t.Fatalf("expected nil after delete, got %+v err=%v", got, err)
	}
}

// TestSQLiteExtensionRepository_DeleteMCPServerCascadesToken 删除 MCP 服务器行必须一并删除其
// mcp_oauth_tokens 令牌，不留悬空行（server_id 后续被复用时不得读到旧令牌）。
func TestSQLiteExtensionRepository_DeleteMCPServerCascadesToken(t *testing.T) {
	db := newMCPOAuthTestDB(t)
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	row := types.MCPServerRow{ID: "srv-del", Name: "srv-del", Transport: "streamable_http", Headers: "{}",
		Enabled: true, Timeout: 30, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPServer(ctx, row); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	tokRow := types.MCPOAuthTokenRow{ServerID: "srv-del", Issuer: "https://as.example.com", AccessTokenEnc: "v1:x",
		TokenType: "Bearer", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPOAuthToken(ctx, tokRow); err != nil {
		t.Fatalf("UpsertMCPOAuthToken: %v", err)
	}

	if err := r.DeleteMCPServer(ctx, "srv-del"); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}
	if got, err := r.GetMCPOAuthToken(ctx, "srv-del"); err != nil || got != nil {
		t.Fatalf("expected token deleted alongside server, got %+v err=%v", got, err)
	}
	if got, err := r.GetMCPServer(ctx, "srv-del"); err != nil || got != nil {
		t.Fatalf("expected server row deleted, got %+v err=%v", got, err)
	}
}

// TestSQLiteExtensionRepository_UninstallCleanup_MCPCascadesToken UninstallCleanup(extType="mcp")
// 删除独立连接器时，同样必须清理其 OAuth 令牌。
func TestSQLiteExtensionRepository_UninstallCleanup_MCPCascadesToken(t *testing.T) {
	db := newMCPOAuthTestDB(t)
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	row := types.MCPServerRow{ID: "srv-uninstall", Name: "srv-uninstall", Transport: "streamable_http", Headers: "{}",
		Enabled: true, Timeout: 30, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPServer(ctx, row); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	tokRow := types.MCPOAuthTokenRow{ServerID: "srv-uninstall", Issuer: "https://as.example.com", AccessTokenEnc: "v1:x",
		TokenType: "Bearer", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPOAuthToken(ctx, tokRow); err != nil {
		t.Fatalf("UpsertMCPOAuthToken: %v", err)
	}

	if err := r.UninstallCleanup(ctx, "", "srv-uninstall", "mcp"); err != nil {
		t.Fatalf("UninstallCleanup: %v", err)
	}
	if got, err := r.GetMCPOAuthToken(ctx, "srv-uninstall"); err != nil || got != nil {
		t.Fatalf("expected token cleaned up, got %+v err=%v", got, err)
	}
}

// TestSQLiteExtensionRepository_UninstallCleanup_PluginCascadesToken 插件卸载（extType="plugin"）
// 按 plugin_id 删除其子 MCP 服务器时，同样级联清理这些服务器的 OAuth 令牌。
func TestSQLiteExtensionRepository_UninstallCleanup_PluginCascadesToken(t *testing.T) {
	db := newMCPOAuthTestDB(t)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS plugins (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS skills (name TEXT PRIMARY KEY, plugin_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hook_trust (source_key TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS plugin_user_config (plugin_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS plugin_channels (plugin_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS plugin_app_bindings (plugin_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	row := types.MCPServerRow{ID: "srv-child", Name: "srv-child", PluginID: "plug-1", Transport: "streamable_http", Headers: "{}",
		Enabled: true, Timeout: 30, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPServer(ctx, row); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}
	tokRow := types.MCPOAuthTokenRow{ServerID: "srv-child", Issuer: "https://as.example.com", AccessTokenEnc: "v1:x",
		TokenType: "Bearer", UpdatedAt: "2026-01-01T00:00:00Z"}
	if err := r.UpsertMCPOAuthToken(ctx, tokRow); err != nil {
		t.Fatalf("UpsertMCPOAuthToken: %v", err)
	}

	if err := r.UninstallCleanup(ctx, "plug-1", "", "plugin"); err != nil {
		t.Fatalf("UninstallCleanup: %v", err)
	}
	if got, err := r.GetMCPOAuthToken(ctx, "srv-child"); err != nil || got != nil {
		t.Fatalf("expected child token cleaned up, got %+v err=%v", got, err)
	}
}
