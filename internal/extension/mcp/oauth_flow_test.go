package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// newFlowTestRepo 建一个含 015(mcp_servers) + 040(mcp_oauth_*) 的内存库，供端到端授权流程
// 测试使用；SSoT DDL 建表，列漂移即报错。
func newFlowTestRepo(t *testing.T) *repo.SQLiteExtensionRepository {
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
	return repo.NewSQLiteExtensionRepository(db)
}

// fakeAuthServer 假授权服务器：提供 AS 元数据 well-known 与 /token 端点。
type fakeAuthServer struct {
	*httptest.Server
	issSupported  atomic.Bool
	lastTokenForm atomic.Pointer[url.Values]
	accessToken   atomic.Pointer[string] // /token 应答的 access_token（授权码交换与刷新可分别设置）
	refreshToken  atomic.Pointer[string]
	tokenErr      atomic.Pointer[string] // 非空时 /token 返回该 OAuth 错误码
	tokenCalls    atomic.Int32
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	t.Helper()
	fas := &fakeAuthServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, authServerMetadata{
			Issuer:                        issuerFor(r),
			AuthorizationEndpoint:         issuerFor(r) + "/authorize",
			TokenEndpoint:                 issuerFor(r) + "/token",
			CodeChallengeMethodsSupported: []string{"S256"},
			AuthorizationResponseIssParameterSupported: fas.issSupported.Load(),
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := r.Form
		fas.lastTokenForm.Store(&form)
		fas.tokenCalls.Add(1)
		if e := fas.tokenErr.Load(); e != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, oauthErrorBody{Error: *e})
			return
		}
		access := "access-1"
		if a := fas.accessToken.Load(); a != nil {
			access = *a
		}
		refresh := "refresh-1"
		if rt := fas.refreshToken.Load(); rt != nil {
			refresh = *rt
		}
		writeJSON(w, tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: 3600, RefreshToken: refresh})
	})
	fas.Server = httptest.NewServer(mux)
	t.Cleanup(fas.Close)
	return fas
}

// fakeMCPServer 假 MCP 服务器（Streamable HTTP）：无/错误 Authorization 返回 401 挑战；
// 匹配 validToken 时按 JSON-RPC 方法名应答最小可用结果。
type fakeMCPServer struct {
	*httptest.Server
	validToken    atomic.Pointer[string]
	authServerURL string
	seenAuth      sync.Map // method -> 收到的 Authorization 头，供断言
}

func newFakeMCPServer(t *testing.T, authServerURL string) *fakeMCPServer {
	t.Helper()
	fms := &fakeMCPServer{authServerURL: authServerURL}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protectedResourceMetadata{Resource: fms.selfURL(), AuthorizationServers: []string{fms.authServerURL}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fms.handleRPC(w, r)
	})
	fms.Server = httptest.NewServer(mux)
	t.Cleanup(fms.Close)
	// 默认接受 "access-1"（newFakeAuthServer 的默认 access_token），使大多数测试无需
	// 显式配置即可让 StartFromDB 重连成功；需要模拟"旧令牌被拒绝"的测试可覆盖此值。
	fms.validToken.Store(strPtr("access-1"))
	return fms
}

func (fms *fakeMCPServer) selfURL() string { return fms.URL }

func (fms *fakeMCPServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	want := fms.validToken.Load()
	got := r.Header.Get("Authorization")
	if want == nil || got != "Bearer "+*want {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+fms.selfURL()+`/.well-known/oauth-protected-resource", scope="mcp:read"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	fms.seenAuth.Store(req.Method, got)
	switch req.Method {
	case "server/discover":
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
	case "initialize":
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-11-25"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	case "tools/list":
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{}}})
	case "tools/call":
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}})
	default:
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
	}
}

func setStr(p *atomic.Pointer[string], v string) { p.Store(&v) }

// newFlowManager 构造一个已装配 repo/cipher/loopback httpClient 的 MCPManager，供授权流程测试使用。
func newFlowManager(t *testing.T) (*MCPManager, *repo.SQLiteExtensionRepository) {
	t.Helper()
	r := newFlowTestRepo(t)
	m := NewMCPManagerWithContext(context.Background(), sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool), loopbackClient(), &mockPolicyGate{})
	m.SetRowSource(r, "")
	vault, err := credential.NewVaultWithKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m.SetCredentialCipher(vault)
	return m, r
}

func insertMCPServerRow(t *testing.T, r *repo.SQLiteExtensionRepository, id, mcpURL, oauthJSON string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	row := types.MCPServerRow{ID: id, Name: id, Transport: "streamable_http", URL: mcpURL, Headers: "{}",
		OAuth: oauthJSON, Enabled: true, Timeout: 30, TrustTier: 3, CreatedAt: now, UpdatedAt: now}
	if err := r.UpsertMCPServer(context.Background(), row); err != nil {
		t.Fatalf("insert mcp server row: %v", err)
	}
}

// TestOAuthE2E_BeginAndCompleteWithBearerFollowup 端到端：Begin 返回的 URL 含
// code_challenge(S256)/resource/state/scope；Complete 用 code_verifier+resource 换令牌、
// 加密落库；随后 GetClient 拿到的连接携带 Bearer 完成握手。
func TestOAuthE2E_BeginAndCompleteWithBearerFollowup(t *testing.T) {
	as := newFakeAuthServer(t)
	as.issSupported.Store(true)
	setStr(&as.accessToken, "access-1")
	mcpSrv := newFakeMCPServer(t, as.URL)

	m, r := newFlowManager(t)
	serverID := "srv-e2e"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, `{"client_id":"preconfigured-client"}`)

	authURL, err := m.BeginAuthorization(context.Background(), serverID, "https://gateway.example.com")
	if err != nil {
		t.Fatalf("BeginAuthorization: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authURL: %v", err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Errorf("missing PKCE S256 challenge in authURL: %s", authURL)
	}
	if q.Get("resource") == "" || q.Get("state") == "" {
		t.Errorf("missing resource/state in authURL: %s", authURL)
	}
	if q.Get("scope") != "mcp:read" {
		// 挑战无先验 scope，PRM 也未声明 scopes_supported，此处允许为空；
		// 断言不因为空 scope 判失败，只在非空时校验内容。
		if q.Get("scope") != "" {
			t.Errorf("unexpected scope: %s", q.Get("scope"))
		}
	}
	state := q.Get("state")

	callbackQuery := url.Values{"state": {state}, "code": {"auth-code-1"}, "iss": {as.URL}}
	gotServerID, err := m.CompleteAuthorization(context.Background(), callbackQuery)
	if err != nil {
		t.Fatalf("CompleteAuthorization: %v", err)
	}
	if gotServerID != serverID {
		t.Errorf("expected serverID %q, got %q", serverID, gotServerID)
	}

	form := as.lastTokenForm.Load()
	if form == nil || form.Get("code_verifier") == "" || form.Get("resource") == "" {
		t.Fatalf("token request missing code_verifier/resource: %+v", form)
	}

	tok, err := r.GetMCPOAuthToken(context.Background(), serverID)
	if err != nil || tok == nil {
		t.Fatalf("expected persisted token, err=%v tok=%v", err, tok)
	}
	if tok.AccessTokenEnc == "access-1" || !strings.HasPrefix(tok.AccessTokenEnc, "v1:") {
		t.Errorf("access token must be encrypted at rest, got %q", tok.AccessTokenEnc)
	}

	client := m.GetClient(serverID)
	if client == nil {
		t.Fatal("expected connected client after CompleteAuthorization (StartFromDB)")
	}
	if _, _, err := client.CallTool(context.Background(), "echo", nil); err != nil {
		t.Fatalf("CallTool after authorization failed: %v", err)
	}
	if auth, _ := mcpSrv.seenAuth.Load("tools/call"); auth != "Bearer access-1" {
		t.Errorf("expected tools/call to carry Bearer access-1, got %v", auth)
	}
}

func strPtr(s string) *string { return &s }

// TestOAuthE2E_IssValidation RFC 9207 §2.4 四种情形。
func TestOAuthE2E_IssValidation(t *testing.T) {
	cases := []struct {
		name        string
		issSupport  bool
		respIss     string
		wantErr     bool
		errContains string
	}{
		{name: "supported+present+match", issSupport: true, wantErr: false},
		{name: "supported+absent", issSupport: true, respIss: "__absent__", wantErr: true},
		{name: "unsupported+present+mismatch", issSupport: false, respIss: "https://attacker.example", wantErr: true},
		{name: "unsupported+absent", issSupport: false, respIss: "__absent__", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			as := newFakeAuthServer(t)
			as.issSupported.Store(tc.issSupport)
			mcpSrv := newFakeMCPServer(t, as.URL)
			m, r := newFlowManager(t)
			serverID := "srv-" + strings.ReplaceAll(strings.ReplaceAll(tc.name, "+", "_"), " ", "_")
			insertMCPServerRow(t, r, serverID, mcpSrv.URL, `{"client_id":"preconfigured-client"}`)

			authURL, err := m.BeginAuthorization(context.Background(), serverID, "https://gateway.example.com")
			if err != nil {
				t.Fatalf("BeginAuthorization: %v", err)
			}
			state := mustQueryParam(t, authURL, "state")

			q := url.Values{"state": {state}, "code": {"code-1"}}
			switch tc.respIss {
			case "":
				q.Set("iss", as.URL)
			case "__absent__":
				// 不设置 iss
			default:
				q.Set("iss", tc.respIss)
			}
			_, err = m.CompleteAuthorization(context.Background(), q)
			if tc.wantErr && err == nil {
				t.Fatal("expected iss validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestOAuthE2E_IssMismatchDoesNotSurfaceError iss 不匹配时不得读取/展示 error/error_description。
func TestOAuthE2E_IssMismatchDoesNotSurfaceError(t *testing.T) {
	as := newFakeAuthServer(t)
	as.issSupported.Store(true)
	mcpSrv := newFakeMCPServer(t, as.URL)
	m, r := newFlowManager(t)
	serverID := "srv-iss-mismatch-error"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, `{"client_id":"preconfigured-client"}`)

	authURL, err := m.BeginAuthorization(context.Background(), serverID, "https://gateway.example.com")
	if err != nil {
		t.Fatalf("BeginAuthorization: %v", err)
	}
	state := mustQueryParam(t, authURL, "state")

	const secretMarker = "SHOULD_NOT_BE_SURFACED_top_secret_reason"
	q := url.Values{"state": {state}, "iss": {"https://attacker.example"}, "error": {"access_denied"}, "error_description": {secretMarker}}
	_, err = m.CompleteAuthorization(context.Background(), q)
	if err == nil {
		t.Fatal("expected iss mismatch error")
	}
	if strings.Contains(err.Error(), secretMarker) {
		t.Errorf("iss-mismatch error must not surface error_description, got: %v", err)
	}
}

func mustQueryParam(t *testing.T, rawURL, key string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	v := u.Query().Get(key)
	if v == "" {
		t.Fatalf("missing query param %q in %s", key, rawURL)
	}
	return v
}

// TestOAuthE2E_StateOneTimeUse state 一次性：第二次用同一 state 必须失败。
func TestOAuthE2E_StateOneTimeUse(t *testing.T) {
	as := newFakeAuthServer(t)
	mcpSrv := newFakeMCPServer(t, as.URL)
	m, r := newFlowManager(t)
	serverID := "srv-onetime"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, `{"client_id":"preconfigured-client"}`)

	authURL, err := m.BeginAuthorization(context.Background(), serverID, "https://gateway.example.com")
	if err != nil {
		t.Fatalf("BeginAuthorization: %v", err)
	}
	state := mustQueryParam(t, authURL, "state")
	q := url.Values{"state": {state}, "code": {"code-1"}}

	if _, err := m.CompleteAuthorization(context.Background(), q); err != nil {
		t.Fatalf("first CompleteAuthorization: %v", err)
	}
	if _, err := m.CompleteAuthorization(context.Background(), q); err == nil {
		t.Fatal("expected error reusing the same state")
	}
}

// TestOAuthBroker_ExpiredStateRejected 过期流程记录必须被拒绝。
func TestOAuthBroker_ExpiredStateRejected(t *testing.T) {
	b := newOAuthBroker()
	b.flows["expired-state"] = &oauthFlowState{ServerID: "srv", CreatedAt: time.Now().Add(-2 * oauthFlowTTL)}
	if _, err := b.takeValid("expired-state"); err == nil {
		t.Fatal("expected expiry error")
	}
	// 一次性：即便过期，取出后也应从表中移除。
	if _, err := b.takeValid("expired-state"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Errorf("expected CodeNotFound on second take, got %v", err)
	}
}

// TestTokenSource_AutoRefreshNearExpiry 过期（含提前 60s 窗口）令牌自动刷新。
func TestTokenSource_AutoRefreshNearExpiry(t *testing.T) {
	as := newFakeAuthServer(t)
	setStr(&as.accessToken, "refreshed-access")
	mcpSrv := newFakeMCPServer(t, as.URL)
	m, r := newFlowManager(t)
	serverID := "srv-autorefresh"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, "{}")

	vault, _ := credential.NewVaultWithKey(make([]byte, 32))
	accessEnc, _ := vault.Encrypt("old-access")
	refreshEnc, _ := vault.Encrypt("refresh-xyz")
	expired := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if err := r.UpsertMCPOAuthToken(context.Background(), types.MCPOAuthTokenRow{
		ServerID: serverID, Issuer: as.URL, Resource: mcpSrv.URL, AccessTokenEnc: accessEnc, RefreshTokenEnc: refreshEnc,
		TokenType: "Bearer", Scopes: "a b", ExpiresAt: expired, UpdatedAt: expired,
	}); err != nil {
		t.Fatal(err)
	}

	ts := m.newTokenSource(serverID)
	got, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "refreshed-access" {
		t.Errorf("expected refreshed access token, got %q", got)
	}
	form := as.lastTokenForm.Load()
	if form == nil || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "refresh-xyz" {
		t.Fatalf("expected refresh_token grant with correct refresh_token, got %+v", form)
	}
}

// TestTokenSource_ConcurrentRefreshSerializedWithClientCredentials 并发撞上过期只刷新一次
// （刷新令牌轮换下重复刷新会失败），且刷新请求携带签发令牌的 client_id 与 DCR client_secret。
func TestTokenSource_ConcurrentRefreshSerializedWithClientCredentials(t *testing.T) {
	as := newFakeAuthServer(t)
	setStr(&as.accessToken, "refreshed-access")
	mcpSrv := newFakeMCPServer(t, as.URL)
	m, r := newFlowManager(t)
	serverID := "srv-concurrent"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, "{}")

	vault, _ := credential.NewVaultWithKey(make([]byte, 32))
	secretEnc, _ := vault.Encrypt("dcr-secret")
	redirect := "http://127.0.0.1:9/oauth/mcp/callback"
	if err := r.UpsertMCPOAuthClient(context.Background(), types.MCPOAuthClientRow{
		Issuer: as.URL, RedirectURI: redirect, ClientID: "dcr-client", ClientSecretEnc: secretEnc, RegistrationMethod: "dcr",
	}); err != nil {
		t.Fatal(err)
	}
	accessEnc, _ := vault.Encrypt("old-access")
	refreshEnc, _ := vault.Encrypt("refresh-xyz")
	expired := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if err := r.UpsertMCPOAuthToken(context.Background(), types.MCPOAuthTokenRow{
		ServerID: serverID, Issuer: as.URL, Resource: mcpSrv.URL, ClientID: "dcr-client", RedirectURI: redirect,
		AccessTokenEnc: accessEnc, RefreshTokenEnc: refreshEnc, TokenType: "Bearer", ExpiresAt: expired, UpdatedAt: expired,
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := m.newTokenSource(serverID).Token(context.Background())
			if err == nil && got != "refreshed-access" {
				err = fmt.Errorf("unexpected token %q", got)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := as.tokenCalls.Load(); n != 1 {
		t.Fatalf("expected exactly one refresh request, got %d", n)
	}
	form := as.lastTokenForm.Load()
	if form.Get("client_id") != "dcr-client" || form.Get("client_secret") != "dcr-secret" {
		t.Fatalf("refresh request missing client credentials: client_id=%q", form.Get("client_id"))
	}
}

// TestClientCall_401RetryOnceAfterRefresh 401 触发一次刷新后重试成功。
func TestClientCall_401RetryOnceAfterRefresh(t *testing.T) {
	as := newFakeAuthServer(t)
	setStr(&as.accessToken, "new-access")
	mcpSrv := newFakeMCPServer(t, as.URL)
	mcpSrv.validToken.Store(strPtr("new-access")) // 服务端只认新令牌，模拟旧令牌已失效

	m, r := newFlowManager(t)
	serverID := "srv-retry-401"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, "{}")

	vault, _ := credential.NewVaultWithKey(make([]byte, 32))
	accessEnc, _ := vault.Encrypt("old-access")
	refreshEnc, _ := vault.Encrypt("refresh-xyz")
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if err := r.UpsertMCPOAuthToken(context.Background(), types.MCPOAuthTokenRow{
		ServerID: serverID, Issuer: as.URL, Resource: mcpSrv.URL, AccessTokenEnc: accessEnc, RefreshTokenEnc: refreshEnc,
		TokenType: "Bearer", Scopes: "", ExpiresAt: future, UpdatedAt: future,
	}); err != nil {
		t.Fatal(err)
	}

	client := NewMCPClient(MCPClientConfig{Transport: MCPStreamableHTTP, URL: mcpSrv.URL, ServerName: serverID, Timeout: 10 * time.Second}, loopbackClient())
	client.SetTokenSource(m.newTokenSource(serverID))

	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize should succeed after one retry: %v", err)
	}
}

// TestRefreshToken_FailureMarksAuthRequired 刷新失败（AS 返回错误）应把服务器标记为需要授权。
func TestRefreshToken_FailureMarksAuthRequired(t *testing.T) {
	as := newFakeAuthServer(t)
	setStr(&as.tokenErr, "invalid_grant")
	mcpSrv := newFakeMCPServer(t, as.URL)
	m, r := newFlowManager(t)
	serverID := "srv-refresh-fail"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, "{}")
	m.entries[serverID] = &mcpEntry{name: serverID}

	vault, _ := credential.NewVaultWithKey(make([]byte, 32))
	accessEnc, _ := vault.Encrypt("old-access")
	refreshEnc, _ := vault.Encrypt("dead-refresh")
	expired := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if err := r.UpsertMCPOAuthToken(context.Background(), types.MCPOAuthTokenRow{
		ServerID: serverID, Issuer: as.URL, Resource: mcpSrv.URL, AccessTokenEnc: accessEnc, RefreshTokenEnc: refreshEnc,
		TokenType: "Bearer", Scopes: "", ExpiresAt: expired, UpdatedAt: expired,
	}); err != nil {
		t.Fatal(err)
	}

	ts := m.newTokenSource(serverID)
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("expected refresh failure error")
	}
	required, _ := m.entries[serverID].authInfo()
	if !required {
		t.Error("expected server marked AuthRequired after refresh failure")
	}
}

// TestAuthRequiredError_InsufficientScopeUnionOnNextBegin 403 insufficient_scope 置位后，
// 再次 BeginAuthorization 的 scope 为「已存令牌 scope ∪ 挑战 scope」。
func TestAuthRequiredError_InsufficientScopeUnionOnNextBegin(t *testing.T) {
	as := newFakeAuthServer(t)
	mcpSrv := newFakeMCPServer(t, as.URL)
	m, r := newFlowManager(t)
	serverID := "srv-stepup"
	insertMCPServerRow(t, r, serverID, mcpSrv.URL, `{"client_id":"preconfigured-client"}`)
	m.entries[serverID] = &mcpEntry{name: serverID}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := r.UpsertMCPOAuthToken(context.Background(), types.MCPOAuthTokenRow{
		ServerID: serverID, Issuer: as.URL, Resource: mcpSrv.URL, AccessTokenEnc: "v1:x", RefreshTokenEnc: "",
		TokenType: "Bearer", Scopes: "scope:read", ExpiresAt: "", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	challenge := wrapAuthChallenge(serverID, &AuthChallengeError{Status: http.StatusForbidden, ErrorCode: "insufficient_scope", Scope: "scope:write"})
	authErr := m.authRequiredError(serverID, serverID, challenge)
	if authErr == nil {
		t.Fatal("expected authRequiredError to return a wrapped error")
	}
	required, scopes := m.entries[serverID].authInfo()
	if !required || len(scopes) != 1 || scopes[0] != "scope:write" {
		t.Fatalf("unexpected auth state: required=%v scopes=%v", required, scopes)
	}

	authURL, err := m.BeginAuthorization(context.Background(), serverID, "https://gateway.example.com")
	if err != nil {
		t.Fatalf("BeginAuthorization: %v", err)
	}
	gotScope := mustQueryParam(t, authURL, "scope")
	fields := strings.Fields(gotScope)
	hasRead, hasWrite := false, false
	for _, f := range fields {
		if f == "scope:read" {
			hasRead = true
		}
		if f == "scope:write" {
			hasWrite = true
		}
	}
	if !hasRead || !hasWrite {
		t.Errorf("expected scope union of read+write, got %q", gotScope)
	}
}

// TestSelectScopes_Union 单元测试覆盖 selectScopes 的并集/优先级逻辑，独立于网络交互。
func TestSelectScopes_Union(t *testing.T) {
	got := selectScopes("chal:a chal:b", []string{"prm:x"}, []string{"chal:a", "old:y"}, []string{"offline_access"})
	want := map[string]bool{"chal:a": true, "chal:b": true, "old:y": true, "offline_access": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want set %v", got, want)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected scope %q in %v", s, got)
		}
	}
}

// TestSelectScopes_PRMFallbackWhenNoChallenge 无挑战 scope 时回退 PRM scopes_supported。
func TestSelectScopes_PRMFallbackWhenNoChallenge(t *testing.T) {
	got := selectScopes("", []string{"prm:x", "prm:y"}, nil, nil)
	if len(got) != 2 {
		t.Fatalf("expected PRM scopes as fallback, got %v", got)
	}
}
