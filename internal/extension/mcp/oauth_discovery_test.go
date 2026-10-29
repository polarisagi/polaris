package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/network"
)

// loopbackClient 测试专用：真正允许连接 127.0.0.1 的 SafeHTTPClient（httptest.Server 监听地址），
// 与生产装配 ollama/embedding 等本机服务时使用的工厂一致，不是绕过 SafeDialer 的临时手段。
func loopbackClient() network.SafeHTTPClient {
	return network.NewLoopbackSafeHTTPClient(config.DefaultThresholds().M11Policy)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestFetchProtectedResourceMetadata_ResourceMetadataGiven 401 挑战直接给出 resource_metadata
// URL 时应直接取该 URL，不走 well-known 回退。
func TestFetchProtectedResourceMetadata_ResourceMetadataGiven(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom-prm" {
			t.Errorf("unexpected path %s, well-known fallback should not be tried", r.URL.Path)
		}
		writeJSON(w, protectedResourceMetadata{Resource: "http://mcp.example.com/mcp", AuthorizationServers: []string{"http://as.example.com"}})
	}))
	defer srv.Close()

	mcpURL := mustParseURL(t, "http://mcp.example.com/mcp")
	doc, err := fetchProtectedResourceMetadata(context.Background(), loopbackClient(), mcpURL, srv.URL+"/custom-prm", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != "http://as.example.com" {
		t.Errorf("unexpected doc: %+v", doc)
	}
}

// TestFetchProtectedResourceMetadata_WellKnownFallback 无 resource_metadata 时按
// basic_authorization_discovery.md 顺序尝试子路径 well-known → 根 well-known。
func TestFetchProtectedResourceMetadata_WellKnownFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protectedResourceMetadata{AuthorizationServers: []string{"http://as.example.com"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mcpURL := mustParseURL(t, srv.URL+"/mcp")
	doc, err := fetchProtectedResourceMetadata(context.Background(), loopbackClient(), mcpURL, "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(doc.AuthorizationServers) != 1 {
		t.Errorf("unexpected doc: %+v", doc)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

// TestDiscoverAuthServerMetadata_RFC8414 issuer 无路径分量时命中 RFC 8414 well-known。
func TestDiscoverAuthServerMetadata_RFC8414(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, authServerMetadata{
			Issuer: issuerFor(r), AuthorizationEndpoint: issuerFor(r) + "/authorize", TokenEndpoint: issuerFor(r) + "/token",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	doc, err := discoverAuthServerMetadata(context.Background(), loopbackClient(), srv.URL, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		t.Errorf("unexpected doc: %+v", doc)
	}
}

// TestDiscoverAuthServerMetadata_OIDC issuer 只在 OIDC well-known 提供元数据时也能发现
// （RFC 8414 well-known 404，回退 openid-configuration）。
func TestDiscoverAuthServerMetadata_OIDC(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", http.NotFound)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, authServerMetadata{
			Issuer: issuerFor(r), AuthorizationEndpoint: issuerFor(r) + "/authorize", TokenEndpoint: issuerFor(r) + "/token",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	doc, err := discoverAuthServerMetadata(context.Background(), loopbackClient(), srv.URL, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Issuer != srv.URL {
		t.Errorf("unexpected issuer: %s", doc.Issuer)
	}
}

// TestDiscoverAuthServerMetadata_PathInsertion issuer 带路径分量时依次尝试
// oauth-authorization-server 插入路径 → openid-configuration 插入路径 → openid-configuration 追加路径。
func TestDiscoverAuthServerMetadata_PathInsertion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant1/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("should not reach append-path variant when insertion variant exists")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	issuer := srv.URL + "/tenant1"
	mux.HandleFunc("/.well-known/openid-configuration/tenant1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, authServerMetadata{
			Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})

	doc, err := discoverAuthServerMetadata(context.Background(), loopbackClient(), issuer, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Issuer != issuer {
		t.Errorf("unexpected issuer: %s", doc.Issuer)
	}
}

// TestDiscoverAuthServerMetadata_IssuerMismatchRejected RFC 8414 §3.3：文档 issuer 与请求
// 使用的 issuer 不一致必须拒绝（防止攻击者的 AS 冒充目标 issuer）。
func TestDiscoverAuthServerMetadata_IssuerMismatchRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, authServerMetadata{
			Issuer: "http://honest.example", AuthorizationEndpoint: "http://honest.example/authorize",
			TokenEndpoint: "http://honest.example/token", CodeChallengeMethodsSupported: []string{"S256"},
		})
	})
	mux.HandleFunc("/.well-known/openid-configuration", http.NotFound)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := discoverAuthServerMetadata(context.Background(), loopbackClient(), srv.URL, true); err == nil {
		t.Fatal("expected error for issuer mismatch")
	}
}

// TestDiscoverAuthServerMetadata_MissingS256Rejected 缺少 code_challenge_methods_supported
// （或不含 S256）必须拒绝（basic_authorization_security.md §Authorization Code Protection）。
func TestDiscoverAuthServerMetadata_MissingS256Rejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, authServerMetadata{Issuer: issuerFor(r), AuthorizationEndpoint: issuerFor(r) + "/authorize", TokenEndpoint: issuerFor(r) + "/token"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := discoverAuthServerMetadata(context.Background(), loopbackClient(), srv.URL, true); err == nil {
		t.Fatal("expected error for missing code_challenge_methods_supported")
	}
}

// TestRequireHTTPSOrLoopback_NonHTTPSRejectedWhenNotLoopback 非回环场景下 http:// 必须拒绝。
func TestRequireHTTPSOrLoopback_NonHTTPSRejectedWhenNotLoopback(t *testing.T) {
	if err := requireHTTPSOrLoopback("http://as.example.com/.well-known/oauth-authorization-server", false); err == nil {
		t.Fatal("expected error for non-https, non-loopback url")
	}
	if err := requireHTTPSOrLoopback("https://as.example.com/.well-known/oauth-authorization-server", false); err != nil {
		t.Errorf("unexpected error for https url: %v", err)
	}
	if err := requireHTTPSOrLoopback("http://127.0.0.1:9999/.well-known/oauth-authorization-server", true); err != nil {
		t.Errorf("unexpected error for loopback http url: %v", err)
	}
}

func TestCanonicalResourceURI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://MCP.Example.com/mcp", "https://mcp.example.com/mcp"},
		{"https://mcp.example.com/", "https://mcp.example.com"},
		{"https://mcp.example.com", "https://mcp.example.com"},
		{"https://mcp.example.com/mcp#frag", "https://mcp.example.com/mcp"},
	}
	for _, tc := range cases {
		got, err := canonicalResourceURI(tc.in)
		if err != nil {
			t.Fatalf("canonicalResourceURI(%q) error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("canonicalResourceURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func issuerFor(r *http.Request) string {
	return "http://" + r.Host
}
