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
