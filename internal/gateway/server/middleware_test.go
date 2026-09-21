package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
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

// TestCheckAuth 覆盖 ADR-0096 决策五的五条分支及其负向面。
//
// 这批用例钉死的是"取消回环豁免"这件事本身：在它落地之前，下面「本机无凭证写操作」
// 一条是**通过**的，而那正是桌面版不可接受的前提——本机任意进程都能拿到完整权限。
func TestCheckAuth(t *testing.T) {
	const localTok = "local-token-abcdef"
	newServer := func() *Server {
		s := &Server{}
		s.SetLocalToken(localTok)
		return s
	}
	call := func(s *Server, r *http.Request, clientIP, expectedKey string) (*httptest.ResponseRecorder, context.Context, bool) {
		w := httptest.NewRecorder()
		ctx, ok := s.checkAuth(w, r, clientIP, expectedKey, NewAuthManager(context.Background()))
		return w, ctx, ok
	}

	t.Run("健康端点免鉴权", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/healthz", nil)
		if _, _, ok := call(newServer(), r, "1.1.1.1", "secret"); !ok {
			t.Error("/healthz 应免鉴权")
		}
	})

	// 静态外壳免鉴权是 Cookie 下发的前提：Cookie 由服务端随首页返回，
	// 若首页本身要鉴权，第一次访问永远拿不到 Cookie（首屏死锁）。
	t.Run("静态外壳免鉴权", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/index.html", nil)
		if _, _, ok := call(newServer(), r, "1.1.1.1", ""); !ok {
			t.Error("静态外壳应免鉴权")
		}
	})

	t.Run("_admin 不属于静态外壳", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/_admin/kill", nil)
		r.RemoteAddr = "127.0.0.1:51234"
		if _, _, ok := call(newServer(), r, "127.0.0.1", ""); ok {
			t.Error("/_admin 必须鉴权，不得被静态外壳分支放行")
		}
	})

	// 核心回归：取消回环豁免。
	t.Run("本机无凭证写操作被拒", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/v1/plugins/install", nil)
		r.RemoteAddr = "127.0.0.1:51234"
		w, _, ok := call(newServer(), r, "127.0.0.1", "")
		if ok {
			t.Error("回环无凭证不得再取得权限（ADR-0096 决策五）")
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("应为 401，实际 %d", w.Code)
		}
	})

	t.Run("远程无凭证被拒", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/v1/plugins/install", nil)
		r.RemoteAddr = "8.8.8.8:51234"
		if _, _, ok := call(newServer(), r, "8.8.8.8", ""); ok {
			t.Error("远程无凭证必须拒绝")
		}
	})

	t.Run("API Key 通过且身份为 api", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/v1/test", nil)
		r.Header.Set("Authorization", "Bearer secret")
		_, ctx, ok := call(newServer(), r, "1.1.1.1", "secret")
		if !ok {
			t.Fatal("正确的 API Key 应通过")
		}
		if ac := authcontext.FromContext(ctx); ac == nil || ac.ClientType != authcontext.ClientTypeAPI {
			t.Errorf("身份应为 api，实际 %+v", ac)
		}
	})

	t.Run("本地令牌请求头通过且本地可信", func(t *testing.T) {
		for _, h := range []struct{ k, v string }{
			{"Authorization", "Bearer " + localTok},
			{"X-API-Key", localTok},
		} {
			r := httptest.NewRequest("POST", "/v1/sessions", nil)
			r.Header.Set(h.k, h.v)
			_, ctx, ok := call(newServer(), r, "127.0.0.1", "")
			if !ok {
				t.Fatalf("%s 携带本地令牌应通过", h.k)
			}
			ac := authcontext.FromContext(ctx)
			if ac == nil || !ac.ClientType.IsLocalTrusted() || !ac.Authenticated {
				t.Errorf("%s：身份应为已认证的本地可信客户端，实际 %+v", h.k, ac)
			}
		}
	})

	t.Run("错误令牌被拒", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/v1/sessions", nil)
		r.Header.Set("X-API-Key", "wrong-token")
		r.RemoteAddr = "127.0.0.1:51234"
		if _, _, ok := call(newServer(), r, "127.0.0.1", ""); ok {
			t.Error("错误令牌必须拒绝")
		}
	})

	t.Run("Cookie 同源通过", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/v1/sessions", nil)
		r.Host = "127.0.0.1:28888"
		r.Header.Set("Origin", "http://127.0.0.1:28888")
		r.AddCookie(&http.Cookie{Name: localTokenCookie, Value: localTok})
		_, ctx, ok := call(newServer(), r, "127.0.0.1", "")
		if !ok {
			t.Fatal("同源 Cookie 应通过")
		}
		if ac := authcontext.FromContext(ctx); ac == nil || !ac.ClientType.IsLocalTrusted() {
			t.Errorf("Cookie 身份应本地可信，实际 %+v", ac)
		}
	})

	// CSRF：跨站页面即便令牌正确也不得成功（正常情况下 SameSite=Strict 已挡住，
	// 本条防的是 Cookie 策略被浏览器实现差异削弱的情况）。
	t.Run("Cookie 跨源被拒", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/v1/plugins/install", nil)
		r.Host = "127.0.0.1:28888"
		r.Header.Set("Origin", "http://evil.example")
		r.AddCookie(&http.Cookie{Name: localTokenCookie, Value: localTok})
		w, _, ok := call(newServer(), r, "127.0.0.1", "")
		if ok {
			t.Error("跨源 Cookie 必须拒绝")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("应为 403，实际 %d", w.Code)
		}
	})

	t.Run("错误 Cookie 被拒", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/v1/sessions", nil)
		r.AddCookie(&http.Cookie{Name: localTokenCookie, Value: "wrong"})
		if _, _, ok := call(newServer(), r, "127.0.0.1", ""); ok {
			t.Error("错误 Cookie 必须拒绝")
		}
	})

	t.Run("逃生阀开启后回环放行", func(t *testing.T) {
		t.Setenv(anonymousLoopbackEnvKey, "1")
		r := httptest.NewRequest("POST", "/v1/plugins/install", nil)
		r.RemoteAddr = "127.0.0.1:51234"
		r.Host = "127.0.0.1:28888"
		_, ctx, ok := call(newServer(), r, "127.0.0.1", "")
		if !ok {
			t.Fatal("逃生阀开启且对端回环时应放行")
		}
		if ac := authcontext.FromContext(ctx); ac == nil || ac.Authenticated {
			t.Errorf("逃生阀身份应为未认证匿名，实际 %+v", ac)
		}
	})

	// DNS rebinding：对端确实是回环，但浏览器认定的 Host 是攻击者域名。
	t.Run("逃生阀不吃 DNS rebinding", func(t *testing.T) {
		t.Setenv(anonymousLoopbackEnvKey, "1")
		r := httptest.NewRequest("POST", "/v1/plugins/install", nil)
		r.RemoteAddr = "127.0.0.1:51234"
		r.Host = "evil.example"
		if _, _, ok := call(newServer(), r, "127.0.0.1", ""); ok {
			t.Error("Host 非回环时逃生阀不得放行（DNS rebinding）")
		}
	})

	// 关键回归（沿用原用例）：伪造 X-Forwarded-For 不得解锁任何分支。
	t.Run("伪造 XFF 不解锁逃生阀", func(t *testing.T) {
		t.Setenv(anonymousLoopbackEnvKey, "1")
		r := httptest.NewRequest("POST", "/v1/plugins/install", nil)
		r.RemoteAddr = "203.0.113.9:51234"
		r.Host = "127.0.0.1:28888"
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		if _, _, ok := call(newServer(), r, "127.0.0.1", ""); ok {
			t.Error("伪造 X-Forwarded-For 回环不得解锁逃生阀")
		}
	})

	// 令牌未注入（嵌入式用法/未经 cmd/polaris 装配）时必须整体 fail-closed。
	t.Run("未注入令牌时一律拒绝", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/v1/sessions", nil)
		r.RemoteAddr = "127.0.0.1:51234"
		r.Header.Set("X-API-Key", "")
		if _, _, ok := call(&Server{}, r, "127.0.0.1", ""); ok {
			t.Error("未配置任何凭证时必须拒绝，不得退化为放行")
		}
	})
}

// issueLocalTokenCookie 只对回环对端下发令牌：远程访问者能看到空壳页面，
// 但拿不到凭证。
func TestIssueLocalTokenCookie(t *testing.T) {
	s := &Server{}
	s.SetLocalToken("tok-xyz")

	cases := []struct {
		name       string
		remoteAddr string
		wantCookie bool
	}{
		{"回环对端下发", "127.0.0.1:51234", true},
		{"IPv6 回环下发", "[::1]:51234", true},
		{"远程对端不下发", "203.0.113.9:51234", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remoteAddr
			w := httptest.NewRecorder()
			s.issueLocalTokenCookie(w, r)

			var got string
			for _, c := range w.Result().Cookies() {
				if c.Name == localTokenCookie {
					got = c.Value
					if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
						t.Error("Cookie 必须是 HttpOnly + SameSite=Strict")
					}
				}
			}
			if tc.wantCookie && got != "tok-xyz" {
				t.Errorf("应下发令牌 Cookie，实际 %q", got)
			}
			if !tc.wantCookie && got != "" {
				t.Error("非回环对端不得下发令牌")
			}
		})
	}

	// 未注入令牌时不下发空 Cookie。
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:51234"
	(&Server{}).issueLocalTokenCookie(w, r)
	if len(w.Result().Cookies()) != 0 {
		t.Error("未注入令牌时不应下发 Cookie")
	}
}
