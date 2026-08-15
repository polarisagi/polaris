package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(10, 5) // rate 10/s, max 5

	// First 5 should pass
	for i := 0; i < 5; i++ {
		if !rl.Allow() {
			t.Errorf("expected allow on token %d", i)
		}
	}

	// 6th should fail
	if rl.Allow() {
		t.Errorf("expected deny on token 6")
	}

	// Wait 0.1s to replenish 1 token
	time.Sleep(150 * time.Millisecond)
	if !rl.Allow() {
		t.Errorf("expected allow after replenish")
	}
}

func TestRateLimitManager(t *testing.T) {
	rm := NewRateLimitManager(context.Background(), 1, 2)

	if !rm.Allow("ip1", "test_client") {
		t.Errorf("expected allow")
	}
	if !rm.Allow("ip1", "test_client") {
		t.Errorf("expected allow")
	}
	if rm.Allow("ip1", "test_client") {
		t.Errorf("expected deny")
	}

	if !rm.Allow("ip2", "cli") { // max 100
		t.Errorf("expected allow for different ip")
	}
}

func TestAuthManager(t *testing.T) {
	am := NewAuthManager(context.Background())

	if am.IsLocked("ip1") {
		t.Errorf("expected false initially")
	}

	am.RecordFailure("ip1")
	am.RecordFailure("ip1")
	if am.IsLocked("ip1") {
		t.Errorf("expected false after 2 failures")
	}

	am.RecordFailure("ip1")
	if !am.IsLocked("ip1") {
		t.Errorf("expected true after 3 failures")
	}

	// Another IP
	if am.IsLocked("ip2") {
		t.Errorf("expected false")
	}
	am.RecordSuccess("ip1")
	if am.IsLocked("ip1") {
		t.Errorf("expected false after success")
	}
}

func TestExtractIP(t *testing.T) {
	t.Setenv("POLARIS_TRUSTED_PROXY", "1")
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.1.1.1:123"

	if extractIP(req) != "1.1.1.1" {
		t.Errorf("expected 1.1.1.1")
	}

	req.Header.Set("X-Forwarded-For", "2.2.2.2, 3.3.3.3")
	if extractIP(req) != "3.3.3.3" {
		t.Errorf("expected 3.3.3.3")
	}

	t.Setenv("POLARIS_TRUSTED_PROXY", "0")
	if extractIP(req) != "1.1.1.1" {
		t.Errorf("expected 1.1.1.1 without proxy trust")
	}
}

func TestIsLoopback(t *testing.T) {
	if !isLoopback("127.0.0.1") {
		t.Errorf("expected true")
	}
	if !isLoopback("[::1]") {
		t.Errorf("expected true")
	}
	if isLoopback("8.8.8.8") {
		t.Errorf("expected false")
	}
}

func TestLoggingResponseWriter(t *testing.T) {
	w := httptest.NewRecorder()
	lrw := NewLoggingResponseWriter(w)

	lrw.WriteHeader(404)
	lrw.Write([]byte("not found"))

	if lrw.statusCode != 404 {
		t.Errorf("expected 404")
	}
	if string(lrw.body) != "not found" {
		t.Errorf("expected not found")
	}
	lrw.Flush()
}

func TestHealthPaths(t *testing.T) {
	paths := healthPathSet
	if _, ok := paths["/healthz"]; !ok {
		t.Errorf("expected /healthz")
	}
	if _, ok := paths["/v1/fuzz"]; ok {
		t.Errorf("did not expect /v1/fuzz")
	}
}

func TestCheckAuth(t *testing.T) {
	s := &Server{}
	am := NewAuthManager(context.Background())

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)

	_, ok := s.checkAuth(w, req, "1.1.1.1", "secret", am)
	if !ok {
		t.Errorf("healthz should pass without auth")
	}

	// 零认证模式的回环判定取 TCP 对端（peerIP），不取传入的 clientIP——
	// 后者可能源自 X-Forwarded-For。这里必须显式设置 RemoteAddr，
	// 否则 httptest 默认填的是 192.0.2.1:1234（非回环）。
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/plugins/install", nil)
	req.RemoteAddr = "127.0.0.1:51234"
	_, ok = s.checkAuth(w, req, "127.0.0.1", "", am)
	if !ok {
		t.Errorf("admin write from localhost should pass if no secret")
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/plugins/install", nil)
	req.RemoteAddr = "8.8.8.8:51234"
	_, ok = s.checkAuth(w, req, "8.8.8.8", "", am)
	if ok {
		t.Errorf("admin write from remote should fail if no secret")
	}

	// 关键回归：远程连接 + 伪造 X-Forwarded-For 回环，且 clientIP 已被 extractIP
	// 带偏成 127.0.0.1——鉴权仍必须按 TCP 对端拒绝。
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/plugins/install", nil)
	req.RemoteAddr = "203.0.113.9:51234"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	_, ok = s.checkAuth(w, req, "127.0.0.1", "", am)
	if ok {
		t.Errorf("伪造 X-Forwarded-For 回环不得解锁零认证模式")
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	_, ok = s.checkAuth(w, req, "1.1.1.1", "secret", am)
	if !ok {
		t.Errorf("should pass with correct auth")
	}
}
