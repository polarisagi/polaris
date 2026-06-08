package inference

import (
	"math/rand/v2"
	"sync"
	"time"
)

// ─── 选择策略 ──────────────────────────────────────────────────────────────────

// SelectStrategy 凭证选择策略。
type SelectStrategy int

const (
	// StrategyFillFirst 优先榨干第一个可用凭证再切换（适合单账号多 key 有配额分组场景）。
	StrategyFillFirst SelectStrategy = iota
	// StrategyRoundRobin 依序轮换（均匀分摊 rate limit，最常用）。
	StrategyRoundRobin
	// StrategyRandom 随机选取（避免多实例间同步碰撞）。
	StrategyRandom
	// StrategyLeastUsed 选请求数最少的凭证（均衡长期负载）。
	StrategyLeastUsed
)

// ─── 冷却 TTL ─────────────────────────────────────────────────────────────────

const (
	// cooldownAuth 401 瞬时认证失败冷却期。
	// 5 分钟而非立即永久禁用——单 key 场景 token 可能已轮换。
	cooldownAuth = 5 * time.Minute

	// cooldownHard 402 计费耗尽 / 429 速率限制冷却期。
	cooldownHard = 60 * time.Minute

	// cooldownPermanent 认证永久失败（token 撤销/刷新失败），实际等于"禁用"。
	cooldownPermanent = 30 * 24 * time.Hour
)

// cooldownFor 将 FailReason 映射到对应冷却时长。
// 仅对 ShouldRotateCredential==true 的 reason 返回非零值。
func cooldownFor(r FailReason) time.Duration {
	switch r {
	case ReasonAuth:
		return cooldownAuth
	case ReasonBilling, ReasonRateLimit:
		return cooldownHard
	case ReasonAuthPermanent:
		return cooldownPermanent
	default:
		// ServerError/Timeout/Overloaded 等：不进入凭证冷却，由 CircuitBreaker 处理
		return 0
	}
}

// ─── PooledCredential ─────────────────────────────────────────────────────────

// PooledCredential 池中单个凭证的运行时状态。
// 外部通过 CredFn() 和 RecordResult() 与其交互，不直接读写字段。
type PooledCredential struct {
	key   string // API key 明文（池生命周期内常驻内存）
	label string // 日志可见标识符，不包含 key 内容

	mu            sync.Mutex
	requestCount  int64     // 累计发出的请求数（StrategyLeastUsed 排序依据）
	cooldownUntil time.Time // 冷却期结束时刻；零值表示可用
	lastReason    FailReason
}

// Label 返回日志安全标识（不含 key）。
func (c *PooledCredential) Label() string { return c.label }

// Available 报告该凭证当前是否可用（冷却期已过）。
func (c *PooledCredential) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cooldownUntil.IsZero() || time.Now().After(c.cooldownUntil)
}

// CredFn 返回与 adapter 构造函数兼容的 func() string。
// 每次调用累计 requestCount，adapter 侧仍应 defer clearString(&local) 清零局部拷贝。
func (c *PooledCredential) CredFn() func() string {
	return func() string {
		c.mu.Lock()
		c.requestCount++
		c.mu.Unlock()
		return c.key
	}
}

// RecordResult 根据推理结果更新凭证状态。
//   - err==nil → 记录成功（无操作，保持可用）
//   - err!=nil → Classify 后若 ShouldRotateCredential，设置对应冷却期
func (c *PooledCredential) RecordResult(err error) {
	if err == nil {
		return
	}
	ce := Classify(err)
	if !ce.ShouldRotateCredential {
		return
	}
	dur := cooldownFor(ce.Reason)
	if dur == 0 {
		return
	}
	c.mu.Lock()
	c.lastReason = ce.Reason
	c.cooldownUntil = time.Now().Add(dur)
	c.mu.Unlock()
}

// RecordFailureReason 直接以 FailReason 记录失败，供已分类错误场景使用。
func (c *PooledCredential) RecordFailureReason(r FailReason) {
	dur := cooldownFor(r)
	if dur == 0 {
		return
	}
	c.mu.Lock()
	c.lastReason = r
	c.cooldownUntil = time.Now().Add(dur)
	c.mu.Unlock()
}

// CooldownUntil 返回冷却期结束时刻（可用于日志/监控）。
func (c *PooledCredential) CooldownUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cooldownUntil
}

// ─── CredentialPool ───────────────────────────────────────────────────────────

// CredentialPool 多凭证线程安全池。
//
// 推荐使用模式（带失败反馈）：
//
//	cred := pool.Pick()
//	adapter := NewAnthropicAdapter(model, cred.CredFn(), client)
//	ch, err := adapter.StreamInfer(ctx, req)
//	cred.RecordResult(err)   // 自动分类并设置冷却
//
// 简单替换模式（无失败反馈，仅用于单 key 场景兼容）：
//
//	adapter := NewAnthropicAdapter(model, pool.CredFn(), client)
type CredentialPool struct {
	mu       sync.Mutex
	creds    []*PooledCredential
	strategy SelectStrategy
	rrCursor int // StrategyRoundRobin 游标（受 mu 保护）
}

// NewCredentialPool 用给定 key 列表和策略创建凭证池。
// keys[0] 在 FillFirst 策略下优先级最高；label 为空时自动生成序号标签。
func NewCredentialPool(keys []string, strategy SelectStrategy) *CredentialPool {
	p := &CredentialPool{strategy: strategy}
	for i, k := range keys {
		label := labelFor(i, k)
		p.creds = append(p.creds, &PooledCredential{key: k, label: label})
	}
	return p
}

// NewSingleCredentialPool 单 key 快捷构造，等价于 NewCredentialPool([]string{key}, StrategyFillFirst)。
func NewSingleCredentialPool(key string) *CredentialPool {
	return NewCredentialPool([]string{key}, StrategyFillFirst)
}

// Add 运行时追加凭证（热更新，线程安全）。
func (p *CredentialPool) Add(key, label string) {
	if label == "" {
		label = labelFor(len(p.creds), key)
	}
	cred := &PooledCredential{key: key, label: label}
	p.mu.Lock()
	p.creds = append(p.creds, cred)
	p.mu.Unlock()
}

// Len 返回池中凭证总数（含冷却中的）。
func (p *CredentialPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.creds)
}

// AvailableCount 返回当前可用（未冷却）的凭证数。
func (p *CredentialPool) AvailableCount() int {
	p.mu.Lock()
	snap := make([]*PooledCredential, len(p.creds))
	copy(snap, p.creds)
	p.mu.Unlock()

	n := 0
	for _, c := range snap {
		if c.Available() {
			n++
		}
	}
	return n
}

// Pick 按策略选取一个可用凭证。
// 若所有凭证均在冷却期内，返回 nil。
func (p *CredentialPool) Pick() *PooledCredential {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.creds) == 0 {
		return nil
	}

	switch p.strategy {
	case StrategyRoundRobin:
		return p.pickRoundRobin()
	case StrategyRandom:
		return p.pickRandom()
	case StrategyLeastUsed:
		return p.pickLeastUsed()
	default: // StrategyFillFirst
		return p.pickFillFirst()
	}
}

// CredFn 返回与 adapter credentialFn 字段兼容的 func() string。
// 每次调用内部执行一次 Pick()，不做失败反馈。
// 适用于只有一个 key 或不需要失败追踪的场景。
func (p *CredentialPool) CredFn() func() string {
	return func() string {
		c := p.Pick()
		if c == nil {
			return ""
		}
		c.mu.Lock()
		c.requestCount++
		c.mu.Unlock()
		return c.key
	}
}

// ─── 内部选择实现（均在 mu 持有时调用）────────────────────────────────────────

func (p *CredentialPool) pickFillFirst() *PooledCredential {
	now := time.Now()
	for _, c := range p.creds {
		c.mu.Lock()
		ok := c.cooldownUntil.IsZero() || now.After(c.cooldownUntil)
		c.mu.Unlock()
		if ok {
			return c
		}
	}
	return nil
}

func (p *CredentialPool) pickRoundRobin() *PooledCredential {
	n := len(p.creds)
	now := time.Now()
	for i := range n {
		idx := (p.rrCursor + i) % n
		c := p.creds[idx]
		c.mu.Lock()
		ok := c.cooldownUntil.IsZero() || now.After(c.cooldownUntil)
		c.mu.Unlock()
		if ok {
			p.rrCursor = (idx + 1) % n
			return c
		}
	}
	return nil
}

func (p *CredentialPool) pickRandom() *PooledCredential {
	now := time.Now()
	// 收集可用集合后随机选一个
	avail := make([]*PooledCredential, 0, len(p.creds))
	for _, c := range p.creds {
		c.mu.Lock()
		ok := c.cooldownUntil.IsZero() || now.After(c.cooldownUntil)
		c.mu.Unlock()
		if ok {
			avail = append(avail, c)
		}
	}
	if len(avail) == 0 {
		return nil
	}
	return avail[rand.IntN(len(avail))]
}

func (p *CredentialPool) pickLeastUsed() *PooledCredential {
	now := time.Now()
	var chosen *PooledCredential
	var minCount int64 = -1
	for _, c := range p.creds {
		c.mu.Lock()
		ok := c.cooldownUntil.IsZero() || now.After(c.cooldownUntil)
		cnt := c.requestCount
		c.mu.Unlock()
		if !ok {
			continue
		}
		if minCount < 0 || cnt < minCount {
			minCount = cnt
			chosen = c
		}
	}
	return chosen
}

// ─── 工具函数 ─────────────────────────────────────────────────────────────────

// labelFor 生成日志安全标签：显示 key 前 8 位（若 key 足够长）+ 序号。
// 前 8 位通常足以区分同 provider 的不同 key，但不泄露完整凭证。
func labelFor(idx int, key string) string {
	prefix := key
	if len(prefix) > 8 {
		prefix = prefix[:8] + "…"
	}
	if prefix == "" {
		prefix = "<empty>"
	}
	return prefix + "#" + itoa(idx)
}

// itoa 简版 int→string，避免 fmt 依赖。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
