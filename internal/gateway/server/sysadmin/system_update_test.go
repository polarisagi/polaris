package sysadmin

import (
	"net/http/httptest"
	"testing"
)

func TestIsLocalOrigin(t *testing.T) {
	cases := []struct {
		origin string
		valid  bool
	}{
		{"http://localhost:8080", true},
		{"https://127.0.0.1:443", true},
		{"http://[::1]", true},
		{"http://evil.com", false},
		{"https://localhost.evil.com", false},
		{"ftp://localhost", false},
		{"", false},
	}

	for _, c := range cases {
		if isLocalOrigin(c.origin) != c.valid {
			t.Errorf("isLocalOrigin(%q) = %v, expected %v", c.origin, !c.valid, c.valid)
		}
	}
}

func TestRequireLocalOrigin(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)

	// No origin -> allowed (API clients)
	w := httptest.NewRecorder()
	if !requireLocalOrigin(w, req) {
		t.Errorf("expected allowed without Origin")
	}

	// Local origin -> allowed
	req.Header.Set("Origin", "http://localhost:3000")
	w = httptest.NewRecorder()
	if !requireLocalOrigin(w, req) {
		t.Errorf("expected allowed with local Origin")
	}

	// Evil origin -> blocked
	req.Header.Set("Origin", "http://evil.com")
	w = httptest.NewRecorder()
	if requireLocalOrigin(w, req) {
		t.Errorf("expected blocked with cross Origin")
	}
	if w.Code != 403 {
		t.Errorf("expected 403 status")
	}
}
