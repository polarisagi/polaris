package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// newOAuthTestRepo 建一个只含 040_mcp_oauth.sql（mcp_oauth_clients/mcp_oauth_tokens）的
// 内存库，供 resolveClient/registerViaDCR 等测试使用——SSoT DDL 建表，列漂移即报错。
func newOAuthTestRepo(t *testing.T) *repo.SQLiteExtensionRepository {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("040_mcp_oauth.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return repo.NewSQLiteExtensionRepository(db)
}

func testManagerWithRepo(t *testing.T) (*MCPManager, *repo.SQLiteExtensionRepository) {
	t.Helper()
	// loopbackClient()：这批测试用 httptest.Server（127.0.0.1）模拟授权服务器/DCR 端点，
	// 需要放行回环地址；生产装配走 SafeDialer 默认策略不变。
	m := NewMCPManager(nil, loopbackClient(), &mockPolicyGate{})
	r := newOAuthTestRepo(t)
	m.SetRowSource(r, "")
	vault, err := credential.NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m.SetCredentialCipher(vault)
	return m, r
}

// TestResolveClient_Preregistered 行内 oauth.client_id 非空时优先使用预注册客户端，
// 即使 AS 同时支持 CIMD/DCR（basic_authorization_client-registration.md §优先级）。
func TestResolveClient_Preregistered(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	row := types.MCPServerRow{ID: "srv-1", OAuth: `{"client_id":"preconfigured-client-id"}`}
	disc := &oauthDiscovery{Issuer: "https://as.example.com", ASMeta: &authServerMetadata{ClientIDMetadataDocumentSupported: true, RegistrationEndpoint: "https://as.example.com/register"}}

	rc, err := m.resolveClient(context.Background(), row, disc, "https://client.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Method != "preregistered" || rc.ClientID != "preconfigured-client-id" {
		t.Errorf("unexpected result: %+v", rc)
	}
}

// TestResolveClient_PreregisteredIssuerMismatch 行配置了 auth_server_metadata_url 时，
// 发现得到的 issuer 与其不一致必须报错，不得静默用错误凭据尝试授权。
func TestResolveClient_PreregisteredIssuerMismatch(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	row := types.MCPServerRow{ID: "srv-1", OAuth: `{"client_id":"cid","auth_server_metadata_url":"https://configured-as.example.com/.well-known/oauth-authorization-server"}`}
	disc := &oauthDiscovery{Issuer: "https://different-as.example.com", ASMeta: &authServerMetadata{}}

	if _, err := m.resolveClient(context.Background(), row, disc, "https://client.example.com"); err == nil {
		t.Fatal("expected error for issuer mismatch against configured auth_server_metadata_url")
	}
}

// TestResolveClient_CIMD 无预注册客户端、AS 声明支持 CIMD、redirectBase 为 https 且非回环时
// 使用 Client ID Metadata Document（client_id 是自托管 URL）。
func TestResolveClient_CIMD(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	row := types.MCPServerRow{ID: "srv-1", OAuth: "{}"}
	disc := &oauthDiscovery{Issuer: "https://as.example.com", ASMeta: &authServerMetadata{ClientIDMetadataDocumentSupported: true, RegistrationEndpoint: "https://as.example.com/register"}}

	rc, err := m.resolveClient(context.Background(), row, disc, "https://gateway.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Method != "cimd" || rc.ClientID != "https://gateway.example.com/oauth/client-metadata.json" {
		t.Errorf("unexpected result: %+v", rc)
	}
}

// TestResolveClient_CIMDSkippedForLoopback CIMD 要求 redirect 基址 https 且非回环；
// 回环基址即便 AS 支持 CIMD 也必须跳过，落到 DCR。
func TestResolveClient_CIMDSkippedForLoopback(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, dcrResponse{ClientID: "dcr-client-1"})
	}))
	defer srv.Close()

	row := types.MCPServerRow{ID: "srv-1", OAuth: "{}"}
	disc := &oauthDiscovery{Issuer: "https://as.example.com", ASMeta: &authServerMetadata{ClientIDMetadataDocumentSupported: true, RegistrationEndpoint: srv.URL}}

	rc, err := m.resolveClient(context.Background(), row, disc, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Method != "dcr" {
		t.Errorf("expected dcr for loopback redirect base, got %+v", rc)
	}
}

// TestResolveClient_DCR_NativeApplicationType 回环基址走 DCR 时 application_type 必须为
// "native"（basic_authorization_client-registration.md §Application Type and Redirect URI Constraints）。
func TestResolveClient_DCR_NativeApplicationType(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	var gotAppType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSONBody(r, &body)
		gotAppType, _ = body["application_type"].(string)
		writeJSON(w, dcrResponse{ClientID: "native-client-1"})
	}))
	defer srv.Close()

	row := types.MCPServerRow{ID: "srv-1", OAuth: "{}"}
	disc := &oauthDiscovery{Issuer: "https://as.example.com", ASMeta: &authServerMetadata{RegistrationEndpoint: srv.URL}}

	rc, err := m.resolveClient(context.Background(), row, disc, "http://localhost:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Method != "dcr" || rc.ClientID != "native-client-1" {
		t.Errorf("unexpected result: %+v", rc)
	}
	if gotAppType != "native" {
		t.Errorf("expected application_type=native, got %q", gotAppType)
	}
}

// TestResolveClient_DCR_WebApplicationType 非回环基址走 DCR 时 application_type 为 "web"。
func TestResolveClient_DCR_WebApplicationType(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	var gotAppType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSONBody(r, &body)
		gotAppType, _ = body["application_type"].(string)
		writeJSON(w, dcrResponse{ClientID: "web-client-1"})
	}))
	defer srv.Close()

	row := types.MCPServerRow{ID: "srv-1", OAuth: "{}"}
	disc := &oauthDiscovery{Issuer: "https://as.example.com", ASMeta: &authServerMetadata{RegistrationEndpoint: srv.URL}}

	if _, err := m.resolveClient(context.Background(), row, disc, "https://gateway.example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAppType != "web" {
		t.Errorf("expected application_type=web, got %q", gotAppType)
	}
}

// TestResolveClient_DCR_ReusesRegisteredClient 同一 issuer+redirect_uri 第二次不应再 POST 注册，
// 而是复用 mcp_oauth_clients 中已登记的客户端。
func TestResolveClient_DCR_ReusesRegisteredClient(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, dcrResponse{ClientID: "dcr-client-once", ClientSecret: "s3cr3t"})
	}))
	defer srv.Close()

	row := types.MCPServerRow{ID: "srv-1", OAuth: "{}"}
	disc := &oauthDiscovery{Issuer: "https://as.example.com", ASMeta: &authServerMetadata{RegistrationEndpoint: srv.URL}}

	rc1, err := m.resolveClient(context.Background(), row, disc, "https://gateway.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rc2, err := m.resolveClient(context.Background(), row, disc, "https://gateway.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 registration POST, got %d", calls)
	}
	if rc1.ClientID != rc2.ClientID || rc2.ClientSecret != "s3cr3t" {
		t.Errorf("expected reused client with decrypted secret, got %+v / %+v", rc1, rc2)
	}
}

// TestResolveClient_NoOptionAvailable 无预注册、AS 不支持 CIMD、也没有 registration_endpoint 时
// 返回 CodeConflict，提示用户填写 client_id。
func TestResolveClient_NoOptionAvailable(t *testing.T) {
	m, _ := testManagerWithRepo(t)
	row := types.MCPServerRow{ID: "srv-1", OAuth: "{}"}
	disc := &oauthDiscovery{Issuer: "https://as.example.com", ASMeta: &authServerMetadata{}}

	_, err := m.resolveClient(context.Background(), row, disc, "https://gateway.example.com")
	if err == nil {
		t.Fatal("expected error when no registration mechanism is available")
	}
}

// TestClientMetadataDocument 校验生成内容满足 basic_authorization_client-registration.md
// §Client ID Metadata Documents 的必需字段（client_id/client_name/redirect_uris）且 client_id
// 与文档 URL 一致。
func TestClientMetadataDocument(t *testing.T) {
	doc := ClientMetadataDocument("https://gateway.example.com/")
	if doc["client_id"] != "https://gateway.example.com/oauth/client-metadata.json" {
		t.Errorf("unexpected client_id: %v", doc["client_id"])
	}
	if doc["client_name"] != "Polaris" {
		t.Errorf("unexpected client_name: %v", doc["client_name"])
	}
	redirects, ok := doc["redirect_uris"].([]string)
	if !ok || len(redirects) != 1 || redirects[0] != "https://gateway.example.com/oauth/mcp/callback" {
		t.Errorf("unexpected redirect_uris: %v", doc["redirect_uris"])
	}
	if doc["token_endpoint_auth_method"] != "none" {
		t.Errorf("unexpected token_endpoint_auth_method: %v", doc["token_endpoint_auth_method"])
	}
}

func decodeJSONBody(r *http.Request, out any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(out)
}
