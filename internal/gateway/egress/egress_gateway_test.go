package egress

import (
	"net/http"
	"testing"
)

type mockTransport struct{ called bool }

func (m *mockTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	m.called = true
	return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
}

func TestEgressGateway_AllowedDomain(t *testing.T) {
	mt := &mockTransport{}
	gw := NewEgressGateway(mt, DefaultAllowedDomains())
	req, _ := http.NewRequest("GET", "https://api.deepseek.com/v1/chat", nil)
	resp, err := gw.RoundTrip(req)
	if err != nil {
		t.Fatalf("expected allowed: %v", err)
	}
	if resp.StatusCode != 200 || !mt.called {
		t.Fatal("transport not called")
	}
}

func TestEgressGateway_BlockedDomain(t *testing.T) {
	mt := &mockTransport{}
	gw := NewEgressGateway(mt, DefaultAllowedDomains())
	req, _ := http.NewRequest("GET", "https://evil.example.com/steal", nil)
	_, err := gw.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error for unlisted domain")
	}
	if mt.called {
		t.Fatal("transport must not be called for blocked domain")
	}
}

func TestEgressGateway_AddAllowedDomain(t *testing.T) {
	mt := &mockTransport{}
	gw := NewEgressGateway(mt, DefaultAllowedDomains())
	gw.AddAllowedDomain("custom.example.com")
	req, _ := http.NewRequest("GET", "https://custom.example.com/api", nil)
	_, err := gw.RoundTrip(req)
	if err != nil {
		t.Fatalf("dynamically added domain should be allowed: %v", err)
	}
}

// TestEgressGateway_PeerAllowlistExactHostPort ADR-0111：对端（多为 IP）不在域名白名单里，
// 只按登记的精确 host:port 放行；同主机其他端口仍被拦截。真实双实例实测中发现：缺这一层时
// 配对拉取对端 Agent Card 被 egress_gateway 直接拒绝。
func TestEgressGateway_PeerAllowlistExactHostPort(t *testing.T) {
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	gw := NewEgressGateway(inner, []string{"api.example.com"})
	do := func(u string) error {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		resp, err := gw.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := do("http://100.101.102.103:28888/mcp"); err == nil {
		t.Fatal("unregistered peer IP must be blocked")
	}
	gw.SetPeerAllowlist(func(host string, port int) bool { return host == "100.101.102.103" && port == 28888 })
	if err := do("http://100.101.102.103:28888/mcp"); err != nil {
		t.Fatalf("registered peer must pass: %v", err)
	}
	if err := do("http://100.101.102.103:22/"); err == nil {
		t.Fatal("other port on the peer host must stay blocked")
	}
	if err := do("https://api.example.com/v1"); err != nil {
		t.Fatalf("domain allowlist must keep working: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
