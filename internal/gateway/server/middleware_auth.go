package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
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

// healthPathSet 是精确豁免鉴权的健康/指标端点白名单。
// [P1修复] 原 HasSuffix("z") 匹配过宽（任何以 z 结尾的路径均被豁免），
// 改为显式白名单，防止类似 /v1/providers/fuzz 等路径意外跳过鉴权。
//
//nolint:gochecknoglobals
var healthPathSet = map[string]struct{}{
	"/healthz": {},
	"/readyz":  {},
	"/metrics": {},
}

// checkAuth 执行 API Key 校验和匿名写保护，返回注入了身份的 context。
// 校验失败时直接写响应并返回 false，调用方应立即 return。
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request, clientIP, expectedKey string, authManager *AuthManager) (context.Context, bool) {
	ctx := r.Context()

	// 生成 TraceID (req_ 开头)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	traceID := "req_" + hex.EncodeToString(b)

	// 健康/指标端点始终放行（无需鉴权）
	if _, isHealth := healthPathSet[r.URL.Path]; isHealth {
		return authcontext.WithAuthContext(ctx, &authcontext.AuthContext{UserID: "anonymous", ClientType: "unknown", TraceID: traceID}), true
	}

	// 未配置 API Key：仅允许本机回环访问，防止远程未授权调用
	if expectedKey == "" {
		if !isLoopback(clientIP) {
			slog.Warn("http: POLARIS_API_KEY not set, rejecting non-localhost request", "ip", clientIP, "path", r.URL.Path)
			http.Error(w, "403 Forbidden: POLARIS_API_KEY not configured; set it in environment or restrict to localhost", http.StatusForbidden)
			return ctx, false
		}
		// loopback 无 key：视为 webui 场景（页面加载并发多请求），用 webui quota 而非 unknown，
		// 避免首屏并发 GET 打光 unknown 的 burst=20 导致误触 429。
		return authcontext.WithAuthContext(ctx, &authcontext.AuthContext{UserID: "anonymous", ClientType: "webui", TraceID: traceID}), true
	}

	if authManager.IsLocked(clientIP) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "429 Too Many Requests - Auth Cooldown", http.StatusTooManyRequests)
		return ctx, false
	}

	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.Header.Get("X-API-Key")
	}

	// 恒定时间比较防御时序攻击
	if subtle.ConstantTimeCompare([]byte(token), []byte(expectedKey)) != 1 {
		authManager.RecordFailure(clientIP)
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return ctx, false
	}

	authManager.RecordSuccess(clientIP)
	// MVP 阶段单一 API Key，统一记录为 admin
	return authcontext.WithAuthContext(ctx, &authcontext.AuthContext{UserID: "admin", ClientType: "api", TraceID: traceID}), true
}

// withMiddleware 挂载所有基础网关级别的安全防护（Auth + Rate Limit + CORS + Logging + Panic Recovery）
//
//nolint:gocyclo
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	// 按照 M13 规范，为每个 IP 分配一个单独的桶，限制默认并发 QPS
	limiter := NewRateLimitManager(20, 50)
	authManager := NewAuthManager()

	expectedKey := os.Getenv("POLARIS_API_KEY")
	if expectedKey == "" {
		slog.Warn("http: POLARIS_API_KEY not set — non-localhost requests will be rejected with 403; set POLARIS_API_KEY to enable remote access")
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
		clientType := "unknown"
		if authCtx != nil {
			clientType = authCtx.ClientType
		}

		// 注入污点：默认本地客户端 TaintMedium，API 调用（外部网络） TaintHigh
		taint := types.TaintMedium
		if clientType == "api" {
			taint = types.TaintHigh
		}
		ctx = taint.InjectToContext(ctx)

		// readiness 守卫：未就绪前只放行 /healthz, /readyz, /v1/status, /metrics
		alwaysAllow := map[string]bool{
			"/healthz":   true,
			"/readyz":    true,
			"/v1/status": true,
			"/metrics":   true,
		}
		if !s.isReady.Load() && !alwaysAllow[r.URL.Path] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"starting","message":"server is initializing, please retry in a moment"}`))
			return
		}

		if !limiter.Allow(clientIP+":"+clientType, clientType) {
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
