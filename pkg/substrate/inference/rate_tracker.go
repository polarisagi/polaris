package inference

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── 速率限制桶 ────────────────────────────────────────────────────────────────

// RateLimitBucket 表示单个时间窗口（分钟 / 小时）内的请求或 token 配额状态。
// Limit==0 表示该头部未出现在响应中，整个桶为空（零值安全）。
type RateLimitBucket struct {
	Limit      int           // 周期内总配额
	Remaining  int           // 当前剩余配额
	ResetIn    time.Duration // 捕获时刻距重置的时长
	CapturedAt time.Time     // 头部被捕获的时刻
}

// Empty 报告此桶是否未初始化（provider 未返回对应头部）。
func (b RateLimitBucket) Empty() bool { return b.Limit == 0 }

// IsExhausted 报告配额是否已耗尽。
func (b RateLimitBucket) IsExhausted() bool { return !b.Empty() && b.Remaining == 0 }

// UsagePct 返回已使用配额的百分比（0~100）。
func (b RateLimitBucket) UsagePct() float64 {
	if b.Empty() || b.Limit == 0 {
		return 0
	}
	used := max(0, b.Limit-b.Remaining)
	return float64(used) * 100.0 / float64(b.Limit)
}

// AgeSeconds 返回此桶数据距当前已过去的秒数（新鲜度指标）。
func (b RateLimitBucket) AgeSeconds() float64 {
	if b.CapturedAt.IsZero() {
		return 0
	}
	return time.Since(b.CapturedAt).Seconds()
}

// SuggestDelay 若配额已耗尽，返回建议等待时长（已扣除数据年龄）；否则返回 0。
func (b RateLimitBucket) SuggestDelay() time.Duration {
	if !b.IsExhausted() {
		return 0
	}
	delay := b.ResetIn - time.Duration(b.AgeSeconds()*float64(time.Second))
	return max(0, delay)
}

// ─── 速率限制状态 ──────────────────────────────────────────────────────────────

// RateLimitState 汇聚单个 provider 的四个速率限制桶。
type RateLimitState struct {
	Provider        string
	RequestsPerMin  RateLimitBucket
	TokensPerMin    RateLimitBucket
	RequestsPerHour RateLimitBucket
	TokensPerHour   RateLimitBucket
	CapturedAt      time.Time
}

// SuggestDelay 在所有桶中取最大建议等待时长。
// 只要有一个桶耗尽，调用方就应该等待。
func (s *RateLimitState) SuggestDelay() time.Duration {
	var max time.Duration
	for _, b := range []RateLimitBucket{
		s.RequestsPerMin,
		s.TokensPerMin,
		s.RequestsPerHour,
		s.TokensPerHour,
	} {
		if d := b.SuggestDelay(); d > max {
			max = d
		}
	}
	return max
}

// IsExhausted 报告任意桶是否耗尽。
func (s *RateLimitState) IsExhausted() bool {
	return s.RequestsPerMin.IsExhausted() ||
		s.TokensPerMin.IsExhausted() ||
		s.RequestsPerHour.IsExhausted() ||
		s.TokensPerHour.IsExhausted()
}

// ─── 速率限制追踪器 ────────────────────────────────────────────────────────────

// RateLimitTracker 解析并存储各 provider 的速率限制状态，线程安全。
//
// 支持的 12 个头部（大小写不敏感）：
//
//	x-ratelimit-limit-requests          x-ratelimit-limit-tokens
//	x-ratelimit-remaining-requests      x-ratelimit-remaining-tokens
//	x-ratelimit-reset-requests          x-ratelimit-reset-tokens
//	x-ratelimit-limit-requests-1h       x-ratelimit-limit-tokens-1h
//	x-ratelimit-remaining-requests-1h   x-ratelimit-remaining-tokens-1h
//	x-ratelimit-reset-requests-1h       x-ratelimit-reset-tokens-1h
//
// 重置时长格式：Go duration 字符串（"1m30s"）或整数秒（"30"）。
type RateLimitTracker struct {
	mu     sync.RWMutex
	states map[string]*RateLimitState
}

// NewRateLimitTracker 创建一个空的速率限制追踪器。
func NewRateLimitTracker() *RateLimitTracker {
	return &RateLimitTracker{states: make(map[string]*RateLimitState)}
}

// Parse 从 HTTP 响应头解析速率限制信息并存储到追踪器。
// 返回解析出的状态（不管是否有有效头部均返回，空桶为零值）。
func (t *RateLimitTracker) Parse(provider string, h http.Header) *RateLimitState {
	now := time.Now()
	s := &RateLimitState{
		Provider:   provider,
		CapturedAt: now,
	}

	// 分钟窗口
	s.RequestsPerMin = parseBucket(
		h.Get("x-ratelimit-limit-requests"),
		h.Get("x-ratelimit-remaining-requests"),
		h.Get("x-ratelimit-reset-requests"),
		now,
	)
	s.TokensPerMin = parseBucket(
		h.Get("x-ratelimit-limit-tokens"),
		h.Get("x-ratelimit-remaining-tokens"),
		h.Get("x-ratelimit-reset-tokens"),
		now,
	)

	// 小时窗口（-1h 后缀）
	s.RequestsPerHour = parseBucket(
		h.Get("x-ratelimit-limit-requests-1h"),
		h.Get("x-ratelimit-remaining-requests-1h"),
		h.Get("x-ratelimit-reset-requests-1h"),
		now,
	)
	s.TokensPerHour = parseBucket(
		h.Get("x-ratelimit-limit-tokens-1h"),
		h.Get("x-ratelimit-remaining-tokens-1h"),
		h.Get("x-ratelimit-reset-tokens-1h"),
		now,
	)

	t.mu.Lock()
	t.states[provider] = s
	t.mu.Unlock()
	return s
}

// Get 返回指定 provider 上次解析的状态；若无记录，返回 false。
func (t *RateLimitTracker) Get(provider string) (*RateLimitState, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.states[provider]
	return s, ok
}

// ─── 捕获传输层 ────────────────────────────────────────────────────────────────

// RateLimitCapturingTransport 是一个 http.RoundTripper 包装器。
// 它在每次 HTTP 响应后自动将速率限制头部送入 Tracker，无需修改任何 adapter。
//
// 使用方式：
//
//	tracker := inference.NewRateLimitTracker()
//	transport := &inference.RateLimitCapturingTransport{
//	    Inner:    http.DefaultTransport,
//	    Tracker:  tracker,
//	    Provider: "anthropic",
//	}
//	client := &http.Client{Transport: transport}
//	adapter := inference.NewAnthropicAdapter(model, credFn, client)
type RateLimitCapturingTransport struct {
	Inner    http.RoundTripper // 底层传输层（通常为 SafeDialer 包装后的 Transport）
	Tracker  *RateLimitTracker
	Provider string
}

// RoundTrip 实现 http.RoundTripper，透传请求并在成功响应后解析限速头部。
func (t *RateLimitCapturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	inner := t.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(r)
	// 无论成功失败都尝试解析（429 响应头同样携带重置时间）
	if resp != nil && t.Tracker != nil {
		t.Tracker.Parse(t.Provider, resp.Header)
	}
	return resp, err
}

// ─── 退避计算 ──────────────────────────────────────────────────────────────────

// BackoffConfig 去相关抖动指数退避配置。
type BackoffConfig struct {
	Base        time.Duration // 基础延迟，默认 5s
	Max         time.Duration // 最大延迟，默认 120s
	JitterRatio float64       // 抖动比例（0~1），默认 0.5
}

// DefaultBackoff 推荐的生产退避配置。
var DefaultBackoff = BackoffConfig{
	Base:        5 * time.Second,
	Max:         120 * time.Second,
	JitterRatio: 0.5,
}

var delaySeed atomic.Uint64

// Delay 返回第 attempt 次重试（从 1 开始）的建议等待时长。
//
// 算法：
//  1. 指数退避基础值 = min(Base × 2^(attempt-1), Max)
//  2. 去相关种子 = UnixNano XOR (attempt × 0xDEADBEEF) XOR 计数器
//     → 防止同一时刻启动的多个 goroutine 在相同 attempt 下同步重试（雷群效应）
//  3. 抖动 = uniform(0, JitterRatio × 基础值)
//  4. 最终延迟 = min(基础值 + 抖动, Max)
func (c BackoffConfig) Delay(attempt int) time.Duration {
	base := c.Base
	if base <= 0 {
		base = 5 * time.Second
	}
	maxDur := c.Max
	if maxDur <= 0 {
		maxDur = 120 * time.Second
	}
	jr := c.JitterRatio
	if jr <= 0 {
		jr = 0.5
	}

	// 指数退避（位移上溢保护）
	exp := base
	for i := 1; i < attempt && exp < maxDur; i++ {
		next := exp * 2
		if next < exp { // 溢出
			exp = maxDur
			break
		}
		exp = next
	}
	exp = min(exp, maxDur)

	// 去相关种子：混入时间戳和递增计数器避免同步碰撞
	lo := uint64(time.Now().UnixNano()) ^ uint64(attempt)*0xDEADBEEF ^ delaySeed.Add(1)
	hi := lo ^ 0xCAFEBABEDEADC0DE
	rng := rand.New(rand.NewPCG(lo, hi))

	jitter := time.Duration(float64(exp) * jr * rng.Float64())
	return min(exp+jitter, maxDur)
}

// DelayWithState 若 RateLimitState 有有效建议时长，优先使用它；
// 否则退回到 Delay(attempt)。
// 这让调用方在 provider 明确告知重置时间时精确等待，而不是盲目退避。
func (c BackoffConfig) DelayWithState(attempt int, s *RateLimitState) time.Duration {
	if s != nil {
		if d := s.SuggestDelay(); d > 0 {
			return d
		}
	}
	return c.Delay(attempt)
}

// ─── 内部解析辅助 ─────────────────────────────────────────────────────────────

// parseBucket 将三个头部字符串解析为 RateLimitBucket。
// limit/remaining 为整数，reset 为 Go duration 字符串或整数秒。
// 任意字段为空时，对应字段保持零值；若三者均空，返回空桶。
func parseBucket(limitStr, remainStr, resetStr string, capturedAt time.Time) RateLimitBucket {
	if limitStr == "" && remainStr == "" && resetStr == "" {
		return RateLimitBucket{}
	}
	b := RateLimitBucket{CapturedAt: capturedAt}
	b.Limit = parseInt(limitStr)
	b.Remaining = parseInt(remainStr)
	b.ResetIn = parseDuration(resetStr)
	return b
}

// parseInt 将字符串解析为 int，解析失败返回 0。
func parseInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// parseDuration 支持两种重置时长格式：
//   - Go duration 字符串："1m30s"、"30s"、"1h"
//   - 纯整数秒："30"、"3600"
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// 优先尝试 Go duration 格式
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	// 降级：纯整数秒
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Second
	}
	return 0
}
