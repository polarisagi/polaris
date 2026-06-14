package inference

import (
	"fmt"
	"sync"
	"testing"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// ─── 基础构造与可用性 ──────────────────────────────────────────────────────────

func TestNewCredentialPool_Empty(t *testing.T) {
	p := NewCredentialPool(nil, StrategyFillFirst)
	if c := p.Pick(); c != nil {
		t.Fatalf("empty pool Pick() should return nil, got %v", c)
	}
	if p.Len() != 0 {
		t.Fatalf("Len want 0, got %d", p.Len())
	}
}

func TestNewSingleCredentialPool(t *testing.T) {
	p := NewSingleCredentialPool("sk-test-key")
	if p.Len() != 1 {
		t.Fatalf("Len want 1, got %d", p.Len())
	}
	c := p.Pick()
	if c == nil {
		t.Fatal("Pick() should not be nil")
	}
	fn := c.CredFn()
	if got := fn(); string(got) != "sk-test-key" {
		t.Fatalf("CredFn() want %q, got %q", "sk-test-key", got)
	}
}

func TestPool_Add(t *testing.T) {
	p := NewCredentialPool([]string{"key1"}, StrategyFillFirst)
	p.Add("key2", "manual-label")
	if p.Len() != 2 {
		t.Fatalf("Len want 2, got %d", p.Len())
	}
}

func TestPool_AvailableCount(t *testing.T) {
	p := NewCredentialPool([]string{"k1", "k2", "k3"}, StrategyFillFirst)
	if p.AvailableCount() != 3 {
		t.Fatalf("want 3, got %d", p.AvailableCount())
	}

	// 让 k1 进入冷却
	c := p.Pick()
	c.RecordFailureReason(ReasonRateLimit)

	if p.AvailableCount() != 2 {
		t.Fatalf("after cooldown want 2, got %d", p.AvailableCount())
	}
}

// ─── 冷却期逻辑 ────────────────────────────────────────────────────────────────

func TestCooldownFor(t *testing.T) {
	cases := []struct {
		reason   FailReason
		wantGT   time.Duration // 实际冷却 > wantGT
		wantZero bool
	}{
		{ReasonAuth, 4 * time.Minute, false},
		{ReasonRateLimit, 59 * time.Minute, false},
		{ReasonBilling, 59 * time.Minute, false},
		{ReasonAuthPermanent, 29 * 24 * time.Hour, false},
		{ReasonServerError, 0, true}, // 不进入凭证冷却
		{ReasonTimeout, 0, true},
		{ReasonContextOverflow, 0, true},
		{ReasonUnknown, 0, true},
	}
	for _, tc := range cases {
		got := cooldownFor(tc.reason)
		if tc.wantZero {
			if got != 0 {
				t.Errorf("cooldownFor(%s) want 0, got %v", tc.reason, got)
			}
		} else if got <= tc.wantGT {
			t.Errorf("cooldownFor(%s) want > %v, got %v", tc.reason, tc.wantGT, got)
		}
	}
}

func TestCredential_RecordResult_RateLimit(t *testing.T) {
	p := NewSingleCredentialPool("sk-ratelimit")
	c := p.Pick()
	if !c.Available() {
		t.Fatal("should be available before failure")
	}

	// 模拟 429 错误
	err := perrors.New(perrors.CodeInternal, "[INTERNAL] api error (status 429): rate limit exceeded")
	c.RecordResult(err)

	if c.Available() {
		t.Fatal("should be in cooldown after 429")
	}
	until := c.CooldownUntil()
	if until.IsZero() {
		t.Fatal("CooldownUntil should not be zero")
	}
	// 冷却期应在 ~60min 后（允许 1s 误差）
	expected := time.Now().Add(cooldownHard - time.Second)
	if until.Before(expected) {
		t.Errorf("cooldown too short: until=%v, want > %v", until, expected)
	}
}

func TestCredential_RecordResult_Auth(t *testing.T) {
	p := NewSingleCredentialPool("sk-auth")
	c := p.Pick()
	err := perrors.New(perrors.CodeInternal, "[INTERNAL] anthropic: HTTP 401: invalid api key")
	c.RecordResult(err)

	if c.Available() {
		t.Fatal("should be in 5-min cooldown after 401")
	}
	until := c.CooldownUntil()
	// 冷却期应 < 6min
	if until.After(time.Now().Add(6 * time.Minute)) {
		t.Errorf("auth cooldown too long: %v", until)
	}
}

func TestCredential_RecordResult_ServerError_NoCooldown(t *testing.T) {
	p := NewSingleCredentialPool("sk-server")
	c := p.Pick()
	// 500 不进入凭证冷却（由 CircuitBreaker 处理）
	err := perrors.New(perrors.CodeInternal, "[INTERNAL] api error (status 500): internal server error")
	c.RecordResult(err)
	if !c.Available() {
		t.Fatal("server error should NOT put credential into cooldown")
	}
}

func TestCredential_RecordResult_Nil(t *testing.T) {
	p := NewSingleCredentialPool("sk-ok")
	c := p.Pick()
	c.RecordResult(nil) // 成功
	if !c.Available() {
		t.Fatal("successful result should keep credential available")
	}
}

func TestCredential_RecordFailureReason_Permanent(t *testing.T) {
	p := NewSingleCredentialPool("sk-perm")
	c := p.Pick()
	c.RecordFailureReason(ReasonAuthPermanent)
	if c.Available() {
		t.Fatal("permanent auth failure should put credential into very long cooldown")
	}
	// 冷却期应 > 25 天
	if c.CooldownUntil().Before(time.Now().Add(25 * 24 * time.Hour)) {
		t.Fatal("permanent cooldown too short")
	}
}

// ─── 选择策略 ──────────────────────────────────────────────────────────────────

func TestStrategyFillFirst(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1", "k2"}, StrategyFillFirst)
	// FillFirst 应始终先选 k0（只要 k0 可用）
	for range 5 {
		c := p.Pick()
		if c == nil {
			t.Fatal("unexpected nil")
		}
		if string(c.key) != "k0" {
			t.Errorf("FillFirst should always pick k0, got %s", c.key)
		}
	}
}

func TestStrategyFillFirst_Skip_Cooldown(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1", "k2"}, StrategyFillFirst)
	// 将 k0 放入冷却
	p.creds[0].RecordFailureReason(ReasonRateLimit)

	c := p.Pick()
	if c == nil {
		t.Fatal("unexpected nil")
	}
	if string(c.key) != "k1" {
		t.Errorf("FillFirst should skip k0 and pick k1, got %s", c.key)
	}
}

func TestStrategyRoundRobin(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1", "k2"}, StrategyRoundRobin)
	keys := make([]string, 6)
	for i := range 6 {
		c := p.Pick()
		if c == nil {
			t.Fatal("unexpected nil")
		}
		keys[i] = string(c.key)
	}
	// 应轮转 k0 k1 k2 k0 k1 k2
	want := []string{"k0", "k1", "k2", "k0", "k1", "k2"}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("RoundRobin[%d] want %s, got %s", i, k, keys[i])
		}
	}
}

func TestStrategyRoundRobin_Skip_Cooldown(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1", "k2"}, StrategyRoundRobin)
	p.creds[1].RecordFailureReason(ReasonRateLimit) // k1 冷却

	picked := make([]string, 4)
	for i := range 4 {
		c := p.Pick()
		if c == nil {
			t.Fatal("unexpected nil")
		}
		picked[i] = string(c.key)
	}
	// k1 被跳过，应轮转 k0 k2 k0 k2（从 k0 开始，跳 k1）
	for _, k := range picked {
		if k == "k1" {
			t.Errorf("RoundRobin should skip k1 (cooling), got %v", picked)
		}
	}
}

func TestStrategyLeastUsed(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1", "k2"}, StrategyLeastUsed)

	// 手动给 k0 增加 requestCount
	p.creds[0].mu.Lock()
	p.creds[0].requestCount = 100
	p.creds[0].mu.Unlock()

	// 应选 k1 或 k2（count=0）
	c := p.Pick()
	if c == nil {
		t.Fatal("unexpected nil")
	}
	if string(c.key) == "k0" {
		t.Errorf("LeastUsed should skip k0 (count=100), got k0")
	}
}

func TestStrategyRandom(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1", "k2"}, StrategyRandom)
	seen := map[string]bool{}
	for range 200 {
		c := p.Pick()
		if c == nil {
			t.Fatal("unexpected nil")
		}
		seen[string(c.key)] = true
	}
	// 200 次随机选取，理论上三个 key 都会出现
	if len(seen) < 2 {
		t.Errorf("Random should pick multiple keys in 200 tries, got %v", seen)
	}
}

func TestPool_AllCooling_ReturnsNil(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1"}, StrategyFillFirst)
	p.creds[0].RecordFailureReason(ReasonRateLimit)
	p.creds[1].RecordFailureReason(ReasonRateLimit)

	if c := p.Pick(); c != nil {
		t.Fatalf("all credentials cooling, Pick() should return nil, got %v", c)
	}
	if p.AvailableCount() != 0 {
		t.Fatalf("AvailableCount want 0, got %d", p.AvailableCount())
	}
}

// ─── CredFn 兼容模式 ──────────────────────────────────────────────────────────

func TestPool_CredFn_ReturnsKeys(t *testing.T) {
	p := NewCredentialPool([]string{"k0", "k1"}, StrategyRoundRobin)
	fn := p.CredFn()

	got0, got1 := fn(), fn()
	if len(got0) == 0 || len(got1) == 0 {
		t.Fatal("CredFn should return non-empty keys")
	}
}

func TestPool_CredFn_AllCooling_ReturnsEmpty(t *testing.T) {
	p := NewCredentialPool([]string{"k0"}, StrategyFillFirst)
	p.creds[0].RecordFailureReason(ReasonBilling)

	fn := p.CredFn()
	if got := fn(); len(got) != 0 {
		t.Errorf("all cooling CredFn() want empty, got %q", got)
	}
}

// ─── 并发安全 ──────────────────────────────────────────────────────────────────

func TestPool_ConcurrentPick(t *testing.T) {
	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}
	p := NewCredentialPool(keys, StrategyRoundRobin)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			c := p.Pick()
			if c != nil {
				c.RecordResult(nil)
			}
		})
	}
	wg.Wait()
	// 没有 panic / race → 通过
}

// ─── Label 格式 ───────────────────────────────────────────────────────────────

func TestLabelFor(t *testing.T) {
	cases := []struct {
		idx  int
		key  string
		want string
	}{
		{0, "sk-ant-12345678abcdef", "sk-ant-1…#0"},
		{1, "short", "short#1"},
		{2, "", "<empty>#2"},
	}
	for _, tc := range cases {
		got := labelFor(tc.idx, tc.key)
		if got != tc.want {
			t.Errorf("labelFor(%d, %q) = %q, want %q", tc.idx, tc.key, got, tc.want)
		}
	}
}

func TestCredential_Label(t *testing.T) {
	p := NewSingleCredentialPool("sk-secret-key-12345")
	c := p.Pick()
	label := c.Label()
	// label 不得包含完整 key
	if label == "sk-secret-key-12345" {
		t.Errorf("Label should not expose full key, got %q", label)
	}
	// label 应含前缀
	if label == "" {
		t.Error("Label should not be empty")
	}
}
