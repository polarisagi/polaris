package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// mockOAuthCallbackMCPManager 实现 server.MCPManager，只覆盖 CompleteAuthorization
// 路径需要的方法；其余方法均为 no-op（回调 handler 只调用 CompleteAuthorization）。
type mockOAuthCallbackMCPManager struct {
	completeServerID string
	completeErr      error
	completeCalled   bool
	completeQuery    url.Values
}

func (m *mockOAuthCallbackMCPManager) ListServers() []protocol.MCPServerInfo { return nil }
func (m *mockOAuthCallbackMCPManager) Add(context.Context, string, string, protocol.MCPClientConfig) error {
	return nil
}
func (m *mockOAuthCallbackMCPManager) StartFromDB(context.Context, string) error { return nil }
func (m *mockOAuthCallbackMCPManager) Remove(string)                             {}
func (m *mockOAuthCallbackMCPManager) Update(context.Context, protocol.ExtensionRepository, string, protocol.MCPUpdateConfig, string) error {
	return nil
}
func (m *mockOAuthCallbackMCPManager) ApproveNetworkAccess(context.Context, string, protocol.ExtensionRepository, string, bool) error {
	return nil
}
func (m *mockOAuthCallbackMCPManager) IsPluginConnected(string) bool { return false }
func (m *mockOAuthCallbackMCPManager) SetOnToolsChanged(func())      {}
func (m *mockOAuthCallbackMCPManager) BeginAuthorization(context.Context, string, string) (string, error) {
	return "", nil
}
func (m *mockOAuthCallbackMCPManager) CompleteAuthorization(_ context.Context, query url.Values) (string, error) {
	m.completeCalled = true
	m.completeQuery = query
	return m.completeServerID, m.completeErr
}
func (m *mockOAuthCallbackMCPManager) IsServerConnected(string) bool { return false }
func (m *mockOAuthCallbackMCPManager) ReadUIResource(context.Context, string, string) (protocol.UIResource, error) {
	return protocol.UIResource{}, apperr.New(apperr.CodeNotFound, "not implemented in mock")
}
func (m *mockOAuthCallbackMCPManager) CallToolAsApp(context.Context, string, string, map[string]any, string, string) (json.RawMessage, error) {
	return nil, apperr.New(apperr.CodeNotFound, "not implemented in mock")
}
func (m *mockOAuthCallbackMCPManager) ReadResourceAsApp(context.Context, string, string) ([]protocol.MCPResourceContent, error) {
	return nil, apperr.New(apperr.CodeNotFound, "not implemented in mock")
}

func TestHandleMCPOAuthCallback(t *testing.T) {
	t.Run("成功：返回含 postMessage 的 HTML 且 no-store", func(t *testing.T) {
		mgr := &mockOAuthCallbackMCPManager{completeServerID: "srv-1"}
		s := &Server{mcpMgr: mgr}
		req := httptest.NewRequest("GET", "/oauth/mcp/callback?state=abc&code=xyz", nil)
		w := httptest.NewRecorder()
		s.HandleMCPOAuthCallback(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control 应为 no-store，实际 %q", got)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("Referrer-Policy 应为 no-referrer，实际 %q", got)
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "nonce-") {
			t.Errorf("CSP 应包含 nonce，实际 %q", csp)
		}
		body := w.Body.String()
		if !strings.Contains(body, "postMessage") || !strings.Contains(body, "polaris-mcp-oauth") {
			t.Errorf("成功页应包含 postMessage 通知，实际 body=%s", body)
		}
		if !strings.Contains(body, "srv-1") {
			t.Errorf("成功页应带上 serverId，实际 body=%s", body)
		}
		if !mgr.completeCalled {
			t.Error("应调用 CompleteAuthorization")
		}
	})

	t.Run("失败：不回显授权服务器 error_description", func(t *testing.T) {
		sensitive := "attacker controlled description with <script>alert(1)</script>"
		mgr := &mockOAuthCallbackMCPManager{
			completeServerID: "srv-1",
			completeErr:      apperr.New(apperr.CodeUnauthorized, "mcp oauth: authorization server returned error: access_denied "+sensitive),
		}
		s := &Server{mcpMgr: mgr}
		req := httptest.NewRequest("GET", "/oauth/mcp/callback?state=abc&error=access_denied&error_description="+url.QueryEscape(sensitive), nil)
		w := httptest.NewRecorder()
		s.HandleMCPOAuthCallback(w, req)

		body := w.Body.String()
		if strings.Contains(body, sensitive) || strings.Contains(body, "attacker controlled") {
			t.Errorf("失败页不得回显授权服务器 error_description，实际 body=%s", body)
		}
		if strings.Contains(body, "<script>alert") {
			t.Errorf("失败页不得包含未转义的授权服务器数据，实际 body=%s", body)
		}
	})

	t.Run("state 无效：CompleteAuthorization 返回 NotFound，仍不回显 err.Error()", func(t *testing.T) {
		mgr := &mockOAuthCallbackMCPManager{
			completeErr: apperr.New(apperr.CodeNotFound, "mcp oauth: unknown or already-used state"),
		}
		s := &Server{mcpMgr: mgr}
		req := httptest.NewRequest("GET", "/oauth/mcp/callback?state=bogus", nil)
		w := httptest.NewRecorder()
		s.HandleMCPOAuthCallback(w, req)
		if w.Code != http.StatusOK { // 回调页本身始终 200，成败体现在文案里
			t.Fatalf("回调页应始终 200，实际 %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "unknown or already-used state") {
			t.Error("不得回显内部错误消息原文")
		}
	})

	t.Run("MCPMgr 未注入时给出失败页而非 panic", func(t *testing.T) {
		s := &Server{}
		req := httptest.NewRequest("GET", "/oauth/mcp/callback", nil)
		w := httptest.NewRecorder()
		s.HandleMCPOAuthCallback(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", w.Code)
		}
	})
}

func TestHandleMCPOAuthClientMetadata(t *testing.T) {
	t.Run("https 非回环基址返回 200 且 client_id 等于文档 URL", func(t *testing.T) {
		s := &Server{}
		req := httptest.NewRequest("GET", "/oauth/client-metadata.json", nil)
		req.Host = "gateway.example.com"
		req.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		s.HandleMCPOAuthClientMetadata(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type 应为 application/json，实际 %q", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
			t.Errorf("Cache-Control 不符预期，实际 %q", cc)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"client_id":"https://gateway.example.com/oauth/client-metadata.json"`) {
			t.Errorf("client_id 应等于文档自身 URL，实际 body=%s", body)
		}
	})

	t.Run("TLS 终结在反向代理时采信 X-Forwarded-Proto/Host", func(t *testing.T) {
		s := &Server{}
		req := httptest.NewRequest("GET", "/oauth/client-metadata.json", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "polaris.example.com, inner.proxy")
		w := httptest.NewRecorder()
		s.HandleMCPOAuthClientMetadata(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"client_id":"https://polaris.example.com/oauth/client-metadata.json"`) {
			t.Errorf("client_id 应为代理外部地址，实际 body=%s", w.Body.String())
		}
	})

	t.Run("http 基址返回 404", func(t *testing.T) {
		s := &Server{}
		req := httptest.NewRequest("GET", "/oauth/client-metadata.json", nil)
		req.Host = "gateway.example.com"
		w := httptest.NewRecorder()
		s.HandleMCPOAuthClientMetadata(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", w.Code)
		}
	})

	t.Run("回环基址即便 https 也返回 404", func(t *testing.T) {
		s := &Server{}
		req := httptest.NewRequest("GET", "/oauth/client-metadata.json", nil)
		req.Host = "127.0.0.1:28888"
		req.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		s.HandleMCPOAuthClientMetadata(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d", w.Code)
		}
	})
}
