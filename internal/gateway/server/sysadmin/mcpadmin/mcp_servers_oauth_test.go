package mcpadmin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// mockOAuthMCPManager 实现 mcpadmin.MCPManager，只记录调用参数，不做真实连接。
type mockOAuthMCPManager struct {
	beginAuthErr     error
	beginAuthURL     string
	beginRedirectArg string
	beginServerIDArg string

	startFromDBCalledWith string
	startFromDBErr        error
}

func (m *mockOAuthMCPManager) ListServers() []protocol.MCPServerInfo { return nil }
func (m *mockOAuthMCPManager) StartFromDB(_ context.Context, id string) error {
	m.startFromDBCalledWith = id
	return m.startFromDBErr
}
func (m *mockOAuthMCPManager) Update(context.Context, protocol.ExtensionRepository, string, protocol.MCPUpdateConfig, string) error {
	return nil
}
func (m *mockOAuthMCPManager) Remove(string) {}
func (m *mockOAuthMCPManager) ApproveNetworkAccess(context.Context, string, protocol.ExtensionRepository, string, bool) error {
	return nil
}
func (m *mockOAuthMCPManager) BeginAuthorization(_ context.Context, serverID, redirectBase string) (string, error) {
	m.beginServerIDArg = serverID
	m.beginRedirectArg = redirectBase
	if m.beginAuthErr != nil {
		return "", m.beginAuthErr
	}
	return m.beginAuthURL, nil
}

// mockCipher 实现 mcpadmin.CredentialCipher，加密只做可逆的标记，便于测试断言。
type mockCipher struct{ failEncrypt bool }

func (c *mockCipher) Encrypt(plaintext string) (string, error) {
	if c.failEncrypt {
		return "", apperr.New(apperr.CodeInternal, "mock: encrypt failed")
	}
	return "enc:" + plaintext, nil
}

func newOAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS mcp_servers (
			id TEXT PRIMARY KEY, name TEXT, transport TEXT, command TEXT, args TEXT, env TEXT,
			url TEXT, headers TEXT NOT NULL DEFAULT '{}', oauth TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER, timeout INTEGER, trust_tier INTEGER, catalog_id TEXT, plugin_id TEXT,
			work_dir TEXT, requires_network INTEGER, created_at TEXT, updated_at TEXT
		);
		INSERT INTO mcp_servers (id, name, plugin_id, transport, command, args, env, url, headers, oauth, timeout, catalog_id, work_dir, requires_network, trust_tier, enabled, created_at, updated_at)
		VALUES ('srv-1', 'test-remote', '', 'streamable_http', '', '[]', '{}', 'https://example.com/mcp', '{}', '{}', 30, '', '.', 0, 2, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
		CREATE TABLE IF NOT EXISTS plugins (id TEXT PRIMARY KEY, name TEXT, display_name TEXT);
		CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (
			server_id TEXT PRIMARY KEY, issuer TEXT NOT NULL, resource TEXT NOT NULL DEFAULT '',
			client_id TEXT NOT NULL DEFAULT '', redirect_uri TEXT NOT NULL DEFAULT '',
			access_token_enc TEXT NOT NULL DEFAULT '', refresh_token_enc TEXT NOT NULL DEFAULT '',
			token_type TEXT NOT NULL DEFAULT 'Bearer', scopes TEXT NOT NULL DEFAULT '',
			expires_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS mcp_oauth_clients (
			issuer TEXT NOT NULL, redirect_uri TEXT NOT NULL, client_id TEXT NOT NULL,
			client_secret_enc TEXT NOT NULL DEFAULT '', registration_method TEXT NOT NULL DEFAULT 'dcr',
			created_at TEXT NOT NULL DEFAULT '', PRIMARY KEY (issuer, redirect_uri)
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestHandleAuthorizeMCPServer(t *testing.T) {
	db := newOAuthTestDB(t)
	extRepo := repo.NewSQLiteExtensionRepository(db)

	t.Run("MCPMgr 未注入返回 501", func(t *testing.T) {
		h := &MCPAdmin{ExtRepo: extRepo}
		req := httptest.NewRequest("POST", "/v1/mcp-servers/srv-1/oauth/authorize", nil)
		req.SetPathValue("serverID", "srv-1")
		w := httptest.NewRecorder()
		h.HandleAuthorizeMCPServer(w, req)
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("want 501, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("Origin 头存在时用 Origin 作为 redirectBase", func(t *testing.T) {
		mgr := &mockOAuthMCPManager{beginAuthURL: "https://as.example.com/authorize?x=1"}
		h := &MCPAdmin{ExtRepo: extRepo, MCPMgr: mgr}
		req := httptest.NewRequest("POST", "/v1/mcp-servers/srv-1/oauth/authorize", nil)
		req.SetPathValue("serverID", "srv-1")
		req.Header.Set("Origin", "https://gateway.example.com")
		w := httptest.NewRecorder()
		h.HandleAuthorizeMCPServer(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if mgr.beginRedirectArg != "https://gateway.example.com" {
			t.Errorf("redirectBase 应取 Origin，实际 %q", mgr.beginRedirectArg)
		}
		if mgr.beginServerIDArg != "srv-1" {
			t.Errorf("serverID 应透传，实际 %q", mgr.beginServerIDArg)
		}
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["authorization_url"] != mgr.beginAuthURL {
			t.Errorf("响应应带 authorization_url，实际 %+v", resp)
		}
	})

	t.Run("无 Origin 头时用 scheme Host", func(t *testing.T) {
		mgr := &mockOAuthMCPManager{beginAuthURL: "https://as.example.com/authorize"}
		h := &MCPAdmin{ExtRepo: extRepo, MCPMgr: mgr}
		req := httptest.NewRequest("POST", "/v1/mcp-servers/srv-1/oauth/authorize", nil)
		req.SetPathValue("serverID", "srv-1")
		req.Host = "127.0.0.1:28888"
		w := httptest.NewRecorder()
		h.HandleAuthorizeMCPServer(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if mgr.beginRedirectArg != "http://127.0.0.1:28888" {
			t.Errorf("redirectBase 应为 scheme://Host，实际 %q", mgr.beginRedirectArg)
		}
	})

	t.Run("stdio 服务器返回 400", func(t *testing.T) {
		mgr := &mockOAuthMCPManager{beginAuthErr: apperr.New(apperr.CodeInvalidInput, "mcp oauth: not applicable to stdio transport")}
		h := &MCPAdmin{ExtRepo: extRepo, MCPMgr: mgr}
		req := httptest.NewRequest("POST", "/v1/mcp-servers/srv-stdio/oauth/authorize", nil)
		req.SetPathValue("serverID", "srv-stdio")
		w := httptest.NewRecorder()
		h.HandleAuthorizeMCPServer(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestHandlePutMCPServerOAuth(t *testing.T) {
	t.Run("非 https 元数据 URL 返回 400", func(t *testing.T) {
		db := newOAuthTestDB(t)
		h := &MCPAdmin{ExtRepo: repo.NewSQLiteExtensionRepository(db), Cipher: &mockCipher{}}
		body := `{"client_id":"cid","auth_server_metadata_url":"http://as.example.com/.well-known/oauth-authorization-server"}`
		req := httptest.NewRequest("PUT", "/v1/mcp-servers/srv-1/oauth", bytes.NewBufferString(body))
		req.SetPathValue("serverID", "srv-1")
		w := httptest.NewRecorder()
		h.HandlePutMCPServerOAuth(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("设置 client_secret 加密落库且响应不含明文", func(t *testing.T) {
		db := newOAuthTestDB(t)
		extRepo := repo.NewSQLiteExtensionRepository(db)
		h := &MCPAdmin{ExtRepo: extRepo, Cipher: &mockCipher{}}
		body := `{"client_id":"cid","client_secret":"topsecret","auth_server_metadata_url":"https://as.example.com/.well-known/oauth-authorization-server","scopes":["read","write"]}`
		req := httptest.NewRequest("PUT", "/v1/mcp-servers/srv-1/oauth", bytes.NewBufferString(body))
		req.SetPathValue("serverID", "srv-1")
		w := httptest.NewRecorder()
		h.HandlePutMCPServerOAuth(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if bytes.Contains(w.Body.Bytes(), []byte("topsecret")) {
			t.Fatal("响应不得包含明文 secret")
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["has_client_secret"] != true {
			t.Errorf("has_client_secret 应为 true，实际 %+v", resp)
		}

		row, err := extRepo.GetMCPServer(context.Background(), "srv-1")
		if err != nil || row == nil {
			t.Fatalf("get server: %v", err)
		}
		if !bytes.Contains([]byte(row.OAuth), []byte("enc:topsecret")) {
			t.Errorf("落库应为密文，实际 %s", row.OAuth)
		}
		if bytes.Contains([]byte(row.OAuth), []byte("\"topsecret\"")) {
			t.Errorf("落库不得包含明文 secret，实际 %s", row.OAuth)
		}
	})

	t.Run("空 client_secret 保留原值", func(t *testing.T) {
		db := newOAuthTestDB(t)
		extRepo := repo.NewSQLiteExtensionRepository(db)
		h := &MCPAdmin{ExtRepo: extRepo, Cipher: &mockCipher{}}

		// 先设置一个密钥
		body := `{"client_id":"cid","client_secret":"original-secret"}`
		req := httptest.NewRequest("PUT", "/v1/mcp-servers/srv-1/oauth", bytes.NewBufferString(body))
		req.SetPathValue("serverID", "srv-1")
		w := httptest.NewRecorder()
		h.HandlePutMCPServerOAuth(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("初次设置失败: %d %s", w.Code, w.Body.String())
		}

		// 再次 PUT，client_secret 为空字符串（保留原值）
		body2 := `{"client_id":"cid2","client_secret":""}`
		req2 := httptest.NewRequest("PUT", "/v1/mcp-servers/srv-1/oauth", bytes.NewBufferString(body2))
		req2.SetPathValue("serverID", "srv-1")
		w2 := httptest.NewRecorder()
		h.HandlePutMCPServerOAuth(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("二次设置失败: %d %s", w2.Code, w2.Body.String())
		}
		row, err := extRepo.GetMCPServer(context.Background(), "srv-1")
		if err != nil || row == nil {
			t.Fatalf("get server: %v", err)
		}
		if !bytes.Contains([]byte(row.OAuth), []byte("enc:original-secret")) {
			t.Errorf("空 client_secret 应保留原密文，实际 %s", row.OAuth)
		}
	})

	t.Run("显式 null 清除 client_secret", func(t *testing.T) {
		db := newOAuthTestDB(t)
		extRepo := repo.NewSQLiteExtensionRepository(db)
		h := &MCPAdmin{ExtRepo: extRepo, Cipher: &mockCipher{}}

		body := `{"client_id":"cid","client_secret":"will-be-cleared"}`
		req := httptest.NewRequest("PUT", "/v1/mcp-servers/srv-1/oauth", bytes.NewBufferString(body))
		req.SetPathValue("serverID", "srv-1")
		w := httptest.NewRecorder()
		h.HandlePutMCPServerOAuth(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("初次设置失败: %d %s", w.Code, w.Body.String())
		}

		body2 := `{"client_id":"cid","client_secret":null}`
		req2 := httptest.NewRequest("PUT", "/v1/mcp-servers/srv-1/oauth", bytes.NewBufferString(body2))
		req2.SetPathValue("serverID", "srv-1")
		w2 := httptest.NewRecorder()
		h.HandlePutMCPServerOAuth(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("清除失败: %d %s", w2.Code, w2.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["has_client_secret"] != false {
			t.Errorf("清除后 has_client_secret 应为 false，实际 %+v", resp)
		}
		row, err := extRepo.GetMCPServer(context.Background(), "srv-1")
		if err != nil || row == nil {
			t.Fatalf("get server: %v", err)
		}
		if bytes.Contains([]byte(row.OAuth), []byte("client_secret_enc\":\"enc")) {
			t.Errorf("显式 null 后不应仍有密文，实际 %s", row.OAuth)
		}
	})

	t.Run("服务器不存在返回 404", func(t *testing.T) {
		db := newOAuthTestDB(t)
		h := &MCPAdmin{ExtRepo: repo.NewSQLiteExtensionRepository(db), Cipher: &mockCipher{}}
		req := httptest.NewRequest("PUT", "/v1/mcp-servers/missing/oauth", bytes.NewBufferString(`{}`))
		req.SetPathValue("serverID", "missing")
		w := httptest.NewRecorder()
		h.HandlePutMCPServerOAuth(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestHandleDeleteMCPServerOAuthToken(t *testing.T) {
	db := newOAuthTestDB(t)
	extRepo := repo.NewSQLiteExtensionRepository(db)
	seedToken := apptypes.MCPOAuthTokenRow{
		ServerID:       "srv-1",
		Issuer:         "https://as.example.com",
		ClientID:       "cid",
		AccessTokenEnc: "enc:access",
		TokenType:      "Bearer",
		UpdatedAt:      "2026-01-01T00:00:00Z",
	}
	if err := extRepo.UpsertMCPOAuthToken(context.Background(), seedToken); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	mgr := &mockOAuthMCPManager{}
	h := &MCPAdmin{DB: db, ExtRepo: extRepo, MCPMgr: mgr}
	if !listOAuthAuthorized(t, h)["srv-1"] {
		t.Fatal("持有令牌时列表应返回 oauth_authorized=true（与是否预注册无关）")
	}
	req := httptest.NewRequest("DELETE", "/v1/mcp-servers/srv-1/oauth/token", nil)
	req.SetPathValue("serverID", "srv-1")
	w := httptest.NewRecorder()
	h.HandleDeleteMCPServerOAuthToken(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	tok, err := extRepo.GetMCPOAuthToken(context.Background(), "srv-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok != nil {
		t.Error("令牌应已被删除")
	}
	if mgr.startFromDBCalledWith != "srv-1" {
		t.Errorf("应触发重连，实际调用参数 %q", mgr.startFromDBCalledWith)
	}
	if listOAuthAuthorized(t, h)["srv-1"] {
		t.Error("注销后列表应返回 oauth_authorized=false")
	}
}

func listOAuthAuthorized(t *testing.T, h *MCPAdmin) map[string]bool {
	t.Helper()
	w := httptest.NewRecorder()
	h.HandleListMCPServers(w, httptest.NewRequest("GET", "/v1/mcp-servers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Servers []struct {
			ID         string `json:"id"`
			Authorized bool   `json:"oauth_authorized"`
		} `json:"mcp_servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, s := range body.Servers {
		out[s.ID] = s.Authorized
	}
	return out
}

func TestHandleDeleteMCPServerOAuthToken_NotFound(t *testing.T) {
	db := newOAuthTestDB(t)
	h := &MCPAdmin{ExtRepo: repo.NewSQLiteExtensionRepository(db)}
	req := httptest.NewRequest("DELETE", "/v1/mcp-servers/missing/oauth/token", nil)
	req.SetPathValue("serverID", "missing")
	w := httptest.NewRecorder()
	h.HandleDeleteMCPServerOAuthToken(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

// applyOAuthDDL 列表接口联表查询 mcp_oauth_tokens；DDL 取自 schema SSoT，避免手写副本漂移。
func applyOAuthDDL(t *testing.T, db *sql.DB) {
	t.Helper()
	ddl, err := schema.FS.ReadFile("040_mcp_oauth.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply 040_mcp_oauth.sql: %v", err)
	}
}
