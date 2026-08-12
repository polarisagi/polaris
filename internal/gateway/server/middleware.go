package server

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// RateLimiter 基于 Token Bucket 实现单桶限流
type RateLimiter struct {
	mu     sync.Mutex
	tokens int
	last   time.Time
	rate   int
	max    int
}

func NewRateLimiter(rate, max int) *RateLimiter {
	return &RateLimiter{
		tokens: max,
		last:   time.Now(),
		rate:   rate,
		max:    max,
	}
}

func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.last).Seconds()
	rl.tokens += int(elapsed * float64(rl.rate))
	if rl.tokens > rl.max {
		rl.tokens = rl.max
	}
	rl.last = now

	if rl.tokens > 0 {
		rl.tokens--
		return true
	}
	return false
}

// clientQuota 每种客户端类型的限流配额（QPS + burst）。
type clientQuota struct {
	Rate int // QPS
	Max  int // burst 上限
}

// builtinClientQuotas M13 §1.5 配额表（SSoT: state.yaml §m13_gateway）。
func builtinClientQuotas() map[string]clientQuota {
	return map[string]clientQuota{
		"cli":     {Rate: 50, Max: 100},
		"webui":   {Rate: 30, Max: 60},
		"a2a":     {Rate: 30, Max: 60},
		"ws":      {Rate: 5, Max: 10},
		"grpc":    {Rate: 50, Max: 100},
		"admin":   {Rate: 10, Max: 20},
		"api":     {Rate: 30, Max: 60},
		"unknown": {Rate: 10, Max: 20},
	}
}

// RateLimitManager 按标识符（IP/Fingerprint）隔离限流桶
// [P2修复] 原实现 limiters map 无过期清理，长期运行会无界增长（内存泄漏）。
// 每次 Allow 调用时记录最后活跃时间，后台 goroutine 每 5 分钟清理 15 分钟未活跃的条目。
type RateLimitManager struct {
	mu           sync.RWMutex
	limiters     map[string]*RateLimiter
	lastSeen     sync.Map               // string -> time.Time
	clientLimits map[string]clientQuota // clientType → 配额
	defaultRate  int
	defaultMax   int
}

func NewRateLimitManager(parentCtx context.Context, rate, max int) *RateLimitManager {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	rm := &RateLimitManager{
		limiters:     make(map[string]*RateLimiter),
		clientLimits: builtinClientQuotas(),
		defaultRate:  rate,
		defaultMax:   max,
	}
	concurrent.SafeGo(parentCtx, "gateway.server.rate_limit_cleanup_loop", func(ctx context.Context) {
		rm.cleanupLoop(ctx)
	})
	return rm
}

// cleanupLoop 每 5 分钟清理超过 15 分钟未活跃的限流桶，防止内存无界增长。
func (rm *RateLimitManager) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cutoff := time.Now().Add(-15 * time.Minute)
			var keysToDelete []string
			rm.lastSeen.Range(func(key, value any) bool {
				if t, ok := value.(time.Time); ok && t.Before(cutoff) {
					keysToDelete = append(keysToDelete, key.(string))
				}
				return true
			})
			if len(keysToDelete) > 0 {
				rm.mu.Lock()
				for _, key := range keysToDelete {
					if tAny, ok := rm.lastSeen.Load(key); ok {
						if t, isTime := tAny.(time.Time); isTime && t.Before(cutoff) {
							delete(rm.limiters, key)
							rm.lastSeen.Delete(key)
						}
					}
				}
				rm.mu.Unlock()
			}
		case <-ctx.Done():
			return
		}
	}
}

func (rm *RateLimitManager) Allow(key string, clientType string) bool {
	rm.mu.RLock()
	limiter, exists := rm.limiters[key]
	rm.mu.RUnlock()

	if !exists {
		rm.mu.Lock()
		limiter, exists = rm.limiters[key]
		if !exists {
			quota, ok := rm.clientLimits[clientType]
			if !ok {
				quota = clientQuota{Rate: rm.defaultRate, Max: rm.defaultMax}
			}
			limiter = NewRateLimiter(quota.Rate, quota.Max)
			rm.limiters[key] = limiter
		}
		rm.mu.Unlock()
	}

	rm.lastSeen.Store(key, time.Now())

	return limiter.Allow()
}

// AuthManager 管理鉴权防爆破
type AuthManager struct {
	mu       sync.Mutex
	failures map[string]int
	lockedAt map[string]time.Time
}

func NewAuthManager(parentCtx context.Context) *AuthManager {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	am := &AuthManager{
		failures: make(map[string]int),
		lockedAt: make(map[string]time.Time),
	}
	concurrent.SafeGo(parentCtx, "gateway.server.auth_cleanup_loop", func(ctx context.Context) {
		am.cleanupLoop(ctx)
	})
	return am
}

func (am *AuthManager) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			am.mu.Lock()
			for ip, lockedTime := range am.lockedAt {
				if time.Since(lockedTime) > 5*time.Minute {
					delete(am.failures, ip)
					delete(am.lockedAt, ip)
				}
			}
			am.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

func (am *AuthManager) IsLocked(ip string) bool {

	am.mu.Lock()
	defer am.mu.Unlock()

	lockedTime, exists := am.lockedAt[ip]
	if !exists {
		return false
	}
	// 5 分钟冷却
	if time.Since(lockedTime) > 5*time.Minute {
		delete(am.failures, ip)
		delete(am.lockedAt, ip)
		return false
	}
	return true
}

func (am *AuthManager) RecordFailure(ip string) {
	am.mu.Lock()
	defer am.mu.Unlock()

	am.failures[ip]++
	// 连续 3 次失败即锁定
	if am.failures[ip] >= 3 {
		am.lockedAt[ip] = time.Now()
	}
}

func (am *AuthManager) RecordSuccess(ip string) {
	am.mu.Lock()
	defer am.mu.Unlock()

	delete(am.failures, ip)
	delete(am.lockedAt, ip)
}

func extractIP(r *http.Request) string {
	// [P1修复] X-Forwarded-For 可被客户端任意伪造。
	// 仅在显式配置了受信任反向代理（POLARIS_TRUSTED_PROXY=1）时才信任该头部；
	// 否则直接使用 TCP 层 RemoteAddr，防止攻击者通过伪造头绕过 IP 限流/锁定。
	if os.Getenv("POLARIS_TRUSTED_PROXY") == "1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// 取最后一个可信跳（最近的反向代理写入），而非首段（可被客户端控制）
			for i := len(parts) - 1; i >= 0; i-- {
				candidate := strings.TrimSpace(parts[i])
				if candidate != "" {
					return candidate
				}
			}
		}
	}
	// r.RemoteAddr 通常包含端口
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}

// LoggingResponseWriter intercepts HTTP responses to capture the status code and body for logging.
type LoggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	body       []byte
}

func NewLoggingResponseWriter(w http.ResponseWriter) *LoggingResponseWriter {
	return &LoggingResponseWriter{w, http.StatusOK, nil}
}

func (lrw *LoggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *LoggingResponseWriter) Write(b []byte) (int, error) {
	if lrw.statusCode >= 400 {
		lrw.body = append(lrw.body, b...)
	}
	n, err := lrw.ResponseWriter.Write(b)
	if err != nil {
		return n, apperr.Wrap(apperr.CodeInternal, "middleware: ResponseWriter.Write 失败", err)
	}
	return n, nil
}

func (lrw *LoggingResponseWriter) Flush() {
	if flusher, ok := lrw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap 暴露底层 http.ResponseWriter（ADR-0094 决策七）。Go 1.20+ 的
// http.ResponseController（logstream.go / chat/sse.go / openai_compat.go 均用它
// 设置 SetWriteDeadline）依靠标准库 http.rwUnwrapper 接口逐层 Unwrap() 才能定位
// 底层的 http.Flusher / http.Hijacker / SetWriteDeadline 方法。缺了这个方法，
// http.NewResponseController(lrw).SetWriteDeadline(...) 会静默返回
// http.ErrNotSupported，SSE/流式响应的写超时设置全盘失效且不会报错。
func (lrw *LoggingResponseWriter) Unwrap() http.ResponseWriter {
	return lrw.ResponseWriter
}

// 鉴权中间件（adminWritePaths/isAdminWrite/isLoopback/healthPathSet/checkAuth/
// withMiddleware）见 middleware_auth.go（R7 拆分）。
