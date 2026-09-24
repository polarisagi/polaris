package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 鉴权中间件：API Key 校验、匿名写保护、健康端点白名单、withMiddleware 总装
// （R7 拆分自 middleware.go）。限流/失败计数/日志响应包装见 middleware.go。
// ============================================================================

// isLoopback 判断 IP 是否为回环地址（127.x / ::1）。
func isLoopback(ip string) bool {
	// 去掉方括号（IPv6 格式）
	ip = strings.Trim(ip, "[]")
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.IsLoopback()
}

// peerIP 返回 TCP 对端 IP，只取 r.RemoteAddr，**不看任何请求头**。
//
// 与 extractIP 的分工：extractIP 服务于限流/锁定，可以（在显式配置反代时）采信
// X-Forwarded-For；peerIP 服务于**鉴权判定**，输入必须不可被客户端影响。
// 两者不要互相替代——把鉴权建立在可伪造的头上，等于把开关交给攻击者。
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr 不含端口（如某些测试或 Unix socket）时按原值判定。
		return r.RemoteAddr
	}
	return host
}

// healthPathSet 是精确豁免鉴权的健康/指标端点白名单。
// [P1修复] 原 HasSuffix("z") 匹配过宽（任何以 z 结尾的路径均被豁免），
// 改为显式白名单，防止类似 /v1/providers/fuzz 等路径意外跳过鉴权。
//
//nolint:gochecknoglobals
var healthPathSet = map[string]struct{}{
	"/healthz":                     {},
	"/readyz":                      {},
	"/metrics":                     {},
	"/.well-known/agent-card.json": {},
}

// localTokenCookie 是 Web UI 携带本地令牌的 Cookie 名。
//
// 为什么 Web UI 走 Cookie 而不是改前端加请求头：同源 fetch 与 EventSource 自动携带
// Cookie，20 余处调用点与 sse.js 一行不用改；HttpOnly 挡住页面内 JS 读取；
// SameSite=Strict 加上 §checkOrigin 的同源校验挡住 CSRF。令牌经 DNS rebinding
// 也拿不到——重绑定后浏览器认定的源是攻击者域名，不会带上本源的 Cookie。
const localTokenCookie = "polaris_local"

// anonymousLoopbackEnvKey 是取消回环豁免后的逃生阀（ADR-0096 决策五）。
// 默认关闭。它服务的是"本机裸调 /v1/chat/completions 的第三方 OpenAI 兼容客户端"
// 这一存量场景的迁移期，不是长期状态。
const anonymousLoopbackEnvKey = "POLARIS_ALLOW_ANONYMOUS_LOOPBACK"

// presentedToken 取请求携带的令牌（Authorization: Bearer 优先，其次 X-API-Key）。
func presentedToken(r *http.Request) string {
	if v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); v != "" {
		return v
	}
	return r.Header.Get("X-API-Key")
}

// tokenEqual 恒定时间比较，防时序攻击。空串一律不匹配——否则未配置令牌时
// 任何不带凭证的请求都会"匹配成功"。
func tokenEqual(presented, expected string) bool {
	if presented == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// isStaticShellRequest 判定是否为 Web UI 静态外壳请求（SPA 的 HTML/JS/CSS）。
//
// 它必须免鉴权，否则首屏陷入死锁：Cookie 由服务端在返回首页时下发，而首页本身
// 要鉴权才能拿到——第一次访问永远拿不到 Cookie。外壳里没有任何用户数据，数据
// 一律经 /v1 获取；令牌只在 §issueLocalTokenCookie 里对回环对端下发，远程访问者
// 能看到空壳但取不到令牌，所有 /v1 调用仍会 401。
func isStaticShellRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	return !strings.HasPrefix(p, "/v1/") && !strings.HasPrefix(p, "/_admin")
}

// checkOrigin 判定请求来源与目标主机是否同源，用于 Cookie 分支的 CSRF 防护。
//
// 无 Origin 头视为非浏览器客户端（CLI/脚本/curl）——它们不会被跨站诱导发起请求，
// CSRF 的前提不成立。有 Origin 则必须与 Host 完全一致。
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// isLoopbackHost 判定 Host 头指向的是否为本机回环名/地址。
//
// 用于逃生阀分支的 DNS rebinding 防护：重绑定攻击中，浏览器发出的 Host 是攻击者
// 域名（evil.example），即便 TCP 对端确实是 127.0.0.1。只看对端不看 Host 的判定
// 会把这种请求当成本机访问放行。
func isLoopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// newAuthContext 组装带身份的 context。
func newAuthContext(ctx context.Context, userID string, ct authcontext.ClientType, traceID string, authed bool) context.Context {
	return authcontext.WithAuthContext(ctx, &authcontext.AuthContext{
		UserID: userID, ClientType: ct, TraceID: traceID, Authenticated: authed,
	})
}

// checkAuth 执行凭证校验，返回注入了身份的 context。
// 校验失败时直接写响应并返回 false，调用方应立即 return。
//
// 判定顺序固定（ADR-0096 决策五），四条分支之外一律 401：
//
//	① 健康端点与静态外壳 —— 免鉴权（前者供探活，后者是 Cookie 下发的前提）
//	② POLARIS_API_KEY    —— 远程部署凭证，身份 admin / ClientTypeAPI
//	③ 本地令牌（请求头） —— CLI 与桌面外壳，身份 ClientTypeLocal
//	④ 本地令牌（Cookie） —— Web UI，需同源，身份 ClientTypeWebUI
//	⑤ 逃生阀             —— 显式开启 + TCP 对端回环 + Host 回环，匿名
//
// 2026-09-21 取消了"未配置 API Key 时回环即凭证"：那条分支让本机任意进程、以及
// 浏览器页面经简单请求即可取得完整权限。桌面版把这个前提彻底改变了——守护进程
// 会长期在普通用户机器上运行。
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request, clientIP, expectedKey string, authManager *AuthManager) (context.Context, bool) {
	ctx := r.Context()

	// 生成 TraceID (req_ 开头)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	traceID := "req_" + hex.EncodeToString(b)

	// ① 健康/指标端点与静态外壳：始终放行
	if _, isHealth := healthPathSet[r.URL.Path]; isHealth {
		return newAuthContext(ctx, "anonymous", authcontext.ClientTypeUnknown, traceID, false), true
	}
	if isStaticShellRequest(r) {
		return newAuthContext(ctx, "anonymous", authcontext.ClientTypeWebUI, traceID, false), true
	}

	presented := presentedToken(r)

	// 本地令牌先于 IP 冷却判定（ADR-0096 决策五 2026-09-25 追记）：回环上所有本机
	// 客户端共用 127.0.0.1，重启后旧外壳持旧令牌轮询触发的冷却会把持正确令牌的
	// CLI/新窗口一并锁死；本地令牌为随机 256 位，冷却对它无防护价值，只剩误伤。
	// 远程 API Key 可能是用户自设的弱口令，仍在冷却之后判定。
	if actx, ok, handled := s.checkLocalToken(w, r, presented, clientIP, traceID, authManager); handled {
		return actx, ok
	}

	if authManager.IsLocked(clientIP) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "429 Too Many Requests - Auth Cooldown", http.StatusTooManyRequests)
		return ctx, false
	}

	// ② 远程 API Key
	if tokenEqual(presented, expectedKey) {
		authManager.RecordSuccess(clientIP)
		// MVP 阶段单一 API Key，统一记录为 admin
		return newAuthContext(ctx, "admin", authcontext.ClientTypeAPI, traceID, true), true
	}

	// ⑤ 逃生阀：三个条件同时成立才放行，缺一不可。
	if os.Getenv(anonymousLoopbackEnvKey) == "1" && isLoopback(peerIP(r)) && isLoopbackHost(r.Host) {
		// 这里刻意**不用** clientIP（extractIP 的产物），而是直接取 TCP 层 RemoteAddr：
		// extractIP 在 POLARIS_TRUSTED_PROXY=1 时会采信 X-Forwarded-For——该开关的前提是
		// 「前面真的有一个会重写该头的反代」；一旦运营者开了开关却没有真反代（或反代被
		// 绕过直连），攻击者只需发一个 `X-Forwarded-For: 127.0.0.1` 就能让本判定为真。
		// RemoteAddr 由内核填写、不可伪造，用它做鉴权判定，用 clientIP 做限流/锁定
		// （后者被伪造的最坏后果只是限流桶算错，不是越权）。
		return newAuthContext(ctx, "anonymous", authcontext.ClientTypeWebUI, traceID, false), true
	}

	// 只在"确实尝试过凭证"时计失败：无凭证的探测（如尚未拿到 Cookie 的首屏并发
	// 请求）若也计数，本机用户会把自己锁进 429。
	if presented != "" {
		authManager.RecordFailure(clientIP)
	}
	http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
	return ctx, false
}

// issueLocalTokenCookie 向**回环对端**下发本地令牌 Cookie，供 Web UI 后续调用 /v1。
//
// 只认 TCP 对端（peerIP），不认 Host、不认任何请求头：Host 可伪造，而这里要发出去的
// 是等价于完整 API 权限的凭证。远程访问者拿到的是没有 Cookie 的空壳页面。
func (s *Server) issueLocalTokenCookie(w http.ResponseWriter, r *http.Request) {
	token := s.LocalToken()
	if token == "" || !isLoopback(peerIP(r)) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     localTokenCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// withMiddleware 挂载所有基础网关级别的安全防护（Auth + Rate Limit + CORS + Logging + Panic Recovery）
//
//nolint:gocyclo
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	// 按照 M13 规范，为每个 IP 分配一个单独的桶，限制默认并发 QPS
	// 用 s.rootCtx（进程级根 context，NewServer 时由 main.go 的 signal.NotifyContext
	// 传入）而非 context.Background()：否则这两个 cleanupLoop 后台协程在进程收到
	// SIGINT/SIGTERM 时永远不会退出。
	rootCtx := s.rootCtx
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	limiter := NewRateLimitManager(rootCtx, 20, 50)
	authManager := NewAuthManager(rootCtx)

	expectedKey := os.Getenv("POLARIS_API_KEY")
	if expectedKey == "" {
		slog.Info("http: POLARIS_API_KEY 未设置——远程访问不可用；本机客户端用 run/polaris.token 鉴权")
	}
	if os.Getenv(anonymousLoopbackEnvKey) == "1" {
		slog.Warn("http: " + anonymousLoopbackEnvKey + "=1 已开启——本机任意进程可无凭证调用完整 API，仅用于存量客户端迁移期")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			defer r.Body.Close()
		}
		lrw := NewLoggingResponseWriter(w)
		w = lrw

		clientIP := extractIP(r)
		isAPI := strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/healthz"

		sealedException := r.URL.Path == "/_admin/unseal"
		if metrics.GlobalKillswitchStage.Load() >= 3 && !sealedException &&
			r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.URL.Path != "/metrics" {
			w.Header().Set("Retry-After", "3600")
			http.Error(w, "503 Service Unavailable: emergency stop active", http.StatusServiceUnavailable)
			return
		}

		// [P0修复] panic recovery：防止单个 handler panic 导致整个服务崩溃。
		// 捕获 panic 后返回 500，并记录堆栈，服务继续运行。
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("http: handler panic recovered", "method", r.Method, "path", r.URL.Path, "ip", clientIP, "panic", rec)
				// 仅在 Header 尚未写出时写 500，避免重复写头
				if lrw.statusCode == http.StatusOK {
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}
		}()

		defer func() {
			if !isAPI {
				return
			}
			if lrw.statusCode >= 500 {
				slog.Error("http: request failed", "method", r.Method, "path", r.URL.Path, "ip", clientIP, "status", lrw.statusCode, "error", strings.TrimSpace(string(lrw.body)))
			} else if lrw.statusCode >= 400 {
				slog.Warn("http: bad request", "method", r.Method, "path", r.URL.Path, "ip", clientIP, "status", lrw.statusCode, "error", strings.TrimSpace(string(lrw.body)))
			}
		}()

		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-API-Key")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		ctx, ok := s.checkAuth(w, r, clientIP, expectedKey, authManager)
		if !ok {
			return
		}

		authCtx := authcontext.FromContext(ctx)
		clientType := authcontext.ClientTypeUnknown
		if authCtx != nil {
			clientType = authCtx.ClientType
		}

		// 注入污点：默认本地客户端 TaintMedium，API 调用（外部网络） TaintHigh
		taint := types.TaintMedium
		if clientType == authcontext.ClientTypeAPI {
			taint = types.TaintHigh
		}
		ctx = taint.InjectToContext(ctx)

		// readiness 守卫：未就绪前只放行 /healthz, /readyz, /v1/status, /metrics
		alwaysAllow := map[string]bool{
			"/healthz":                     true,
			"/readyz":                      true,
			"/v1/status":                   true,
			"/metrics":                     true,
			"/.well-known/agent-card.json": true,
		}
		if !s.isReady.Load() && !alwaysAllow[r.URL.Path] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"starting","message":"server is initializing, please retry in a moment"}`))
			return
		}

		if !limiter.Allow(clientIP+":"+string(clientType), string(clientType)) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "429 Too Many Requests", http.StatusTooManyRequests)
			return
		}

		if s.rateLimiter != nil && !s.rateLimiter.Allow() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "429 Too Many Requests: global API limit exceeded", http.StatusTooManyRequests)
			return
		}

		if isAPI {
			slog.Debug("http: request", "method", r.Method, "path", r.URL.Path, "ip", clientIP)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// checkLocalToken 本地令牌两条分支（③ 请求头 / ④ Cookie）。handled=false 表示请求
// 未携带有效本地令牌，交由后续分支继续判定。
func (s *Server) checkLocalToken(w http.ResponseWriter, r *http.Request, presented, clientIP, traceID string, authManager *AuthManager) (context.Context, bool, bool) {
	ctx := r.Context()
	// 冷却期内的本地令牌成功不复位该 IP 的失败计数：否则本机正常流量会把针对
	// API Key 的冷却一并清掉，旁路了它对弱口令猜测的限制。
	recordSuccess := func() {
		if !authManager.IsLocked(clientIP) {
			authManager.RecordSuccess(clientIP)
		}
	}
	// ③ 本地令牌（请求头）：CLI / 桌面外壳。身份是 ClientTypeLocal，
	// IsLocalTrusted() 覆盖它——本机已完成令牌校验，与 webui 同级可信。
	localToken := s.LocalToken()
	if tokenEqual(presented, localToken) {
		recordSuccess()
		return newAuthContext(ctx, "local", authcontext.ClientTypeLocal, traceID, true), true, true
	}

	// ④ 本地令牌（Cookie）：Web UI。跨站请求带不上 Strict Cookie，
	// 这里的同源校验是第二道——防的是 Cookie 策略被浏览器实现差异削弱的情况。
	if c, err := r.Cookie(localTokenCookie); err == nil && tokenEqual(c.Value, localToken) {
		if !checkOrigin(r) {
			slog.Warn("http: 拒绝跨源 Cookie 鉴权", "origin", r.Header.Get("Origin"), "host", r.Host, "path", r.URL.Path)
			http.Error(w, "403 Forbidden: cross-origin request rejected", http.StatusForbidden)
			return ctx, false, true
		}
		recordSuccess()
		return newAuthContext(ctx, "local", authcontext.ClientTypeWebUI, traceID, true), true, true
	}

	return ctx, false, false
}
