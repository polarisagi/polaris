package inference

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// ─── parseBucket / parseDuration ──────────────────────────────────────────────

func TestParseDuration(t *testing.T) {
	cases := []struct {
		s    string
		want time.Duration
	}{
		{"1m30s", 90 * time.Second},
		{"30s", 30 * time.Second},
		{"1h", time.Hour},
		{"30", 30 * time.Second}, // 纯整数秒
		{"3600", 3600 * time.Second},
		{"", 0},
		{"invalid", 0},
	}
	for _, tc := range cases {
		got := parseDuration(tc.s)
		if got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestParseBucket_Empty(t *testing.T) {
	b := parseBucket("", "", "", time.Now())
	if !b.Empty() {
		t.Error("all-empty headers should produce empty bucket")
	}
}

func TestParseBucket_Full(t *testing.T) {
	now := time.Now()
	b := parseBucket("1000", "50", "30s", now)
	if b.Limit != 1000 {
		t.Errorf("Limit want 1000, got %d", b.Limit)
	}
	if b.Remaining != 50 {
		t.Errorf("Remaining want 50, got %d", b.Remaining)
	}
	if b.ResetIn != 30*time.Second {
		t.Errorf("ResetIn want 30s, got %v", b.ResetIn)
	}
	if b.CapturedAt != now {
		t.Error("CapturedAt not set")
	}
}

// ─── RateLimitBucket 方法 ─────────────────────────────────────────────────────

func TestBucket_IsExhausted(t *testing.T) {
	b := RateLimitBucket{Limit: 100, Remaining: 0, ResetIn: 30 * time.Second, CapturedAt: time.Now()}
	if !b.IsExhausted() {
		t.Error("remaining=0 should be exhausted")
	}

	b2 := RateLimitBucket{Limit: 100, Remaining: 1}
	if b2.IsExhausted() {
		t.Error("remaining>0 should not be exhausted")
	}

	// 空桶不算耗尽
	if (RateLimitBucket{}).IsExhausted() {
		t.Error("empty bucket should not be exhausted")
	}
}

func TestBucket_UsagePct(t *testing.T) {
	b := RateLimitBucket{Limit: 100, Remaining: 60}
	if got := b.UsagePct(); got != 40.0 {
		t.Errorf("UsagePct want 40.0, got %f", got)
	}
	// 空桶
	if (RateLimitBucket{}).UsagePct() != 0 {
		t.Error("empty bucket UsagePct should be 0")
	}
}

func TestBucket_AgeSeconds(t *testing.T) {
	b := RateLimitBucket{CapturedAt: time.Now().Add(-2 * time.Second)}
	age := b.AgeSeconds()
	if age < 1.5 || age > 3.0 {
		t.Errorf("AgeSeconds want ~2, got %f", age)
	}
}

func TestBucket_SuggestDelay_Exhausted(t *testing.T) {
	b := RateLimitBucket{
		Limit:      100,
		Remaining:  0,
		ResetIn:    60 * time.Second,
		CapturedAt: time.Now(), // 刚捕获，age≈0
	}
	d := b.SuggestDelay()
	// 建议等待应接近 60s
	if d < 55*time.Second || d > 65*time.Second {
		t.Errorf("SuggestDelay want ~60s, got %v", d)
	}
}

func TestBucket_SuggestDelay_NotExhausted(t *testing.T) {
	b := RateLimitBucket{Limit: 100, Remaining: 50, ResetIn: 30 * time.Second, CapturedAt: time.Now()}
	if d := b.SuggestDelay(); d != 0 {
		t.Errorf("not exhausted SuggestDelay should be 0, got %v", d)
	}
}

func TestBucket_SuggestDelay_OldData(t *testing.T) {
	// 捕获时间在 90s 前，ResetIn=60s → 实际等待应为负数，返回 0
	b := RateLimitBucket{
		Limit:      100,
		Remaining:  0,
		ResetIn:    60 * time.Second,
		CapturedAt: time.Now().Add(-90 * time.Second),
	}
	if d := b.SuggestDelay(); d != 0 {
		t.Errorf("stale data SuggestDelay should be 0, got %v", d)
	}
}

// ─── RateLimitTracker.Parse ───────────────────────────────────────────────────

func makeHeader(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestTracker_Parse_NoHeaders(t *testing.T) {
	tr := NewRateLimitTracker()
	s := tr.Parse("openai", http.Header{})
	if s.IsExhausted() {
		t.Error("no headers should not be exhausted")
	}
	if !s.RequestsPerMin.Empty() {
		t.Error("no headers should produce empty bucket")
	}
}

func TestTracker_Parse_PerMinute(t *testing.T) {
	tr := NewRateLimitTracker()
	h := makeHeader(
		"x-ratelimit-limit-requests", "500",
		"x-ratelimit-remaining-requests", "499",
		"x-ratelimit-reset-requests", "1s",
		"x-ratelimit-limit-tokens", "10000",
		"x-ratelimit-remaining-tokens", "9950",
		"x-ratelimit-reset-tokens", "1s",
	)
	s := tr.Parse("openai", h)

	if s.RequestsPerMin.Limit != 500 {
		t.Errorf("RequestsPerMin.Limit want 500, got %d", s.RequestsPerMin.Limit)
	}
	if s.RequestsPerMin.Remaining != 499 {
		t.Errorf("RequestsPerMin.Remaining want 499, got %d", s.RequestsPerMin.Remaining)
	}
	if s.TokensPerMin.Limit != 10000 {
		t.Errorf("TokensPerMin.Limit want 10000, got %d", s.TokensPerMin.Limit)
	}
	if s.RequestsPerHour.Limit != 0 {
		t.Error("no 1h headers → hour bucket should be empty")
	}
}

func TestTracker_Parse_PerHour(t *testing.T) {
	tr := NewRateLimitTracker()
	h := makeHeader(
		"x-ratelimit-limit-requests-1h", "50000",
		"x-ratelimit-remaining-requests-1h", "49000",
		"x-ratelimit-reset-requests-1h", "3600",
		"x-ratelimit-limit-tokens-1h", "1000000",
		"x-ratelimit-remaining-tokens-1h", "900000",
		"x-ratelimit-reset-tokens-1h", "3600",
	)
	s := tr.Parse("anthropic", h)

	if s.RequestsPerHour.Limit != 50000 {
		t.Errorf("RequestsPerHour.Limit want 50000, got %d", s.RequestsPerHour.Limit)
	}
	if s.TokensPerHour.Remaining != 900000 {
		t.Errorf("TokensPerHour.Remaining want 900000, got %d", s.TokensPerHour.Remaining)
	}
}

func TestTracker_Parse_CaseInsensitive(t *testing.T) {
	tr := NewRateLimitTracker()
	// http.Header.Get 会规范化 key，Go net/http 支持大小写不敏感查找
	h := http.Header{}
	h["X-Ratelimit-Limit-Requests"] = []string{"100"}
	h["X-Ratelimit-Remaining-Requests"] = []string{"80"}
	h["X-Ratelimit-Reset-Requests"] = []string{"30s"}

	s := tr.Parse("test", h)
	if s.RequestsPerMin.Limit != 100 {
		t.Errorf("case-insensitive: Limit want 100, got %d", s.RequestsPerMin.Limit)
	}
}

func TestTracker_Get(t *testing.T) {
	tr := NewRateLimitTracker()
	if _, ok := tr.Get("unknown"); ok {
		t.Error("unknown provider should return false")
	}

	tr.Parse("openai", makeHeader(
		"x-ratelimit-limit-requests", "500",
		"x-ratelimit-remaining-requests", "0",
		"x-ratelimit-reset-requests", "60s",
	))

	s, ok := tr.Get("openai")
	if !ok {
		t.Fatal("should find openai after parse")
	}
	if !s.RequestsPerMin.IsExhausted() {
		t.Error("remaining=0 should be exhausted")
	}
}

func TestState_SuggestDelay_FromExhausted(t *testing.T) {
	tr := NewRateLimitTracker()
	h := makeHeader(
		"x-ratelimit-limit-requests", "100",
		"x-ratelimit-remaining-requests", "0",
		"x-ratelimit-reset-requests", "45s",
	)
	s := tr.Parse("openai", h)

	d := s.SuggestDelay()
	if d < 40*time.Second || d > 50*time.Second {
		t.Errorf("SuggestDelay want ~45s, got %v", d)
	}
}

func TestState_IsExhausted(t *testing.T) {
	tr := NewRateLimitTracker()
	// tokens exhausted but requests ok
	h := makeHeader(
		"x-ratelimit-limit-requests", "500",
		"x-ratelimit-remaining-requests", "100",
		"x-ratelimit-reset-requests", "1s",
		"x-ratelimit-limit-tokens", "10000",
		"x-ratelimit-remaining-tokens", "0",
		"x-ratelimit-reset-tokens", "30s",
	)
	s := tr.Parse("openai", h)
	if !s.IsExhausted() {
		t.Error("tokens exhausted → state should be exhausted")
	}
}

// ─── BackoffConfig.Delay ──────────────────────────────────────────────────────

func TestBackoff_MonotonicallyIncreases(t *testing.T) {
	cfg := DefaultBackoff
	prev := time.Duration(0)
	for attempt := 1; attempt <= 8; attempt++ {
		// 运行多次取平均，排除抖动干扰
		var total time.Duration
		const runs = 50
		for range runs {
			total += cfg.Delay(attempt)
		}
		avg := total / runs
		if avg < prev {
			t.Errorf("attempt %d avg %v < attempt %d avg %v (not monotonic)", attempt, avg, attempt-1, prev)
		}
		prev = avg
	}
}

func TestBackoff_RespectsMax(t *testing.T) {
	cfg := BackoffConfig{Base: 5 * time.Second, Max: 30 * time.Second, JitterRatio: 0.5}
	for attempt := 1; attempt <= 20; attempt++ {
		d := cfg.Delay(attempt)
		if d > cfg.Max {
			t.Errorf("attempt %d: delay %v exceeds Max %v", attempt, d, cfg.Max)
		}
	}
}

func TestBackoff_AttemptOne_NearBase(t *testing.T) {
	cfg := BackoffConfig{Base: 5 * time.Second, Max: 120 * time.Second, JitterRatio: 0.5}
	d := cfg.Delay(1)
	// attempt 1: exp=5s, jitter up to 2.5s → total 5~7.5s
	if d < 5*time.Second || d > 8*time.Second {
		t.Errorf("attempt 1 delay want 5~7.5s, got %v", d)
	}
}

func TestBackoff_DecorrelatedJitter(t *testing.T) {
	// 同 attempt 的多次调用应产生不同延迟（去相关抖动）
	cfg := DefaultBackoff
	seen := map[time.Duration]bool{}
	for range 20 {
		seen[cfg.Delay(3)] = true
	}
	if len(seen) < 3 {
		t.Errorf("jitter should produce varied delays, got only %d distinct values", len(seen))
	}
}

func TestBackoff_DelayWithState_UsesProviderHint(t *testing.T) {
	cfg := DefaultBackoff
	tr := NewRateLimitTracker()
	h := makeHeader(
		"x-ratelimit-limit-requests", "100",
		"x-ratelimit-remaining-requests", "0",
		"x-ratelimit-reset-requests", "90s",
	)
	s := tr.Parse("openai", h)

	// provider 告知 90s，应优先于 backoff 计算
	d := cfg.DelayWithState(1, s)
	if d < 85*time.Second {
		t.Errorf("DelayWithState should use provider hint ~90s, got %v", d)
	}
}

func TestBackoff_DelayWithState_FallsBackWhenNoHint(t *testing.T) {
	cfg := BackoffConfig{Base: 5 * time.Second, Max: 120 * time.Second, JitterRatio: 0.5}
	// 无 state
	d := cfg.DelayWithState(1, nil)
	if d < 5*time.Second || d > 8*time.Second {
		t.Errorf("nil state should fall back to Delay(1)~5-7.5s, got %v", d)
	}
}

// ─── 并发安全 ──────────────────────────────────────────────────────────────────

func TestTracker_ConcurrentParse(t *testing.T) {
	tr := NewRateLimitTracker()
	h := makeHeader(
		"x-ratelimit-limit-requests", "1000",
		"x-ratelimit-remaining-requests", "500",
		"x-ratelimit-reset-requests", "60s",
	)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			provider := "p" + itoa(i%5)
			tr.Parse(provider, h)
			tr.Get(provider)
		})
	}
	wg.Wait()
}

// ─── RateLimitCapturingTransport ─────────────────────────────────────────────

type mockTransport struct {
	resp *http.Response
	err  error
}

func (m *mockTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return m.resp, m.err
}

func TestCapturingTransport_ParsesHeaders(t *testing.T) {
	fakeResp := &http.Response{
		StatusCode: 429,
		Header: makeHeader(
			"x-ratelimit-limit-requests", "100",
			"x-ratelimit-remaining-requests", "0",
			"x-ratelimit-reset-requests", "30s",
		),
	}
	tr := NewRateLimitTracker()
	transport := &RateLimitCapturingTransport{
		Inner:    &mockTransport{resp: fakeResp},
		Tracker:  tr,
		Provider: "openai",
	}

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != fakeResp {
		t.Fatal("should return original response unchanged")
	}

	s, ok := tr.Get("openai")
	if !ok {
		t.Fatal("tracker should have openai state after RoundTrip")
	}
	if !s.RequestsPerMin.IsExhausted() {
		t.Error("remaining=0 should be exhausted")
	}
	if s.RequestsPerMin.ResetIn != 30*time.Second {
		t.Errorf("ResetIn want 30s, got %v", s.RequestsPerMin.ResetIn)
	}
}

func TestCapturingTransport_NilTrackerSafe(t *testing.T) {
	fakeResp := &http.Response{StatusCode: 200, Header: http.Header{}}
	transport := &RateLimitCapturingTransport{
		Inner:   &mockTransport{resp: fakeResp},
		Tracker: nil, // nil tracker 不应 panic
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
