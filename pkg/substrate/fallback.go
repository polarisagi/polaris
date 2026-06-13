package substrate

import (
	"context"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// Fallback Chain — 三级降级链 + CircuitBreaker + HealthScorer。
// 架构文档: docs/arch/01-Inference-Runtime-深度选型.md §7

// FallbackTier 降级层级。
type FallbackTier int

const (
	FallbackPrimary   FallbackTier = iota // 首选
	FallbackSecondary                     // 同级备选
	FallbackTertiary                      // 降级备选
	FallbackGraceful                      // 优雅降级
	FallbackEscalate                      // [ESCALATE]
)

// FailMode 失败模式 → 重试策略。
type FailMode string

const (
	FailRateLimit     FailMode = "rate_limit"     // 429 → exponential backoff + 换 provider
	FailServerError   FailMode = "server_error"   // 5xx → 立即换 provider + 冷却
	FailTimeout       FailMode = "timeout"        // → 减少 MaxToken 后重试
	FailContentFilter FailMode = "content_filter" // 400 → 不重试
	FailTokenLimit    FailMode = "token_limit"    // 400 → 压缩 context 后重试
)

// CircuitBreaker 熔断器。
// 状态: Closed → Open → HalfOpen → Closed.
// 5 次连续失败 → Open (10s 冷却) → HalfOpen → 1 次探测成功 → Closed.
type CircuitBreaker struct {
	state            int // 0=Closed, 1=Open, 2=HalfOpen
	failureThreshold int
	cooldownPeriod   time.Duration
	halfOpenMax      int
	consecutiveFails int
	lastFailTime     time.Time
	mu               sync.Mutex
}

// NewCircuitBreaker 创建熔断器。
func NewCircuitBreaker(thresh int, cooldown time.Duration, halfMax int) *CircuitBreaker {
	return &CircuitBreaker{
		failureThreshold: thresh,
		cooldownPeriod:   cooldown,
		halfOpenMax:      halfMax,
	}
}

// RecordHalfOpen 探测。
func (cb *CircuitBreaker) TryHalfOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == 1 && time.Since(cb.lastFailTime) > cb.cooldownPeriod {
		cb.state = 2            // HalfOpen
		cb.consecutiveFails = 0 // use consecutiveFails to track half open attempts maybe, but simplified here
		return true
	}
	return cb.state == 2
}

// Allow 判断是否允许请求通过。
func (cb *CircuitBreaker) Allow() bool {
	return cb.state == 0 || (cb.state == 1 && time.Since(cb.lastFailTime) > cb.cooldownPeriod)
}

// RecordResult 记录调用结果。
func (cb *CircuitBreaker) RecordResult(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if success {
		cb.consecutiveFails = 0
		if cb.state == 2 {
			cb.state = 0
		}
	} else {
		cb.consecutiveFails++
		cb.lastFailTime = time.Now()
		if cb.consecutiveFails >= cb.failureThreshold {
			cb.state = 1
		}
	}
}

// HealthScorer Provider 健康度评分。
// 可用性 40% + 延迟 30% + 成本 20% + 质量 10%.
type HealthScorer struct {
	availabilityWeight float64
	latencyWeight      float64
	costWeight         float64
	qualityWeight      float64
}

// Score 计算健康度分数 (0-1)。
func (hs *HealthScorer) Score(stats *ProviderStats) float64 {
	return hs.availabilityWeight*stats.SuccessRate +
		hs.latencyWeight*(1.0-stats.P95Latency/10000) +
		hs.costWeight*stats.CostAccuracy +
		hs.qualityWeight*stats.QualityScore
}

// ProviderStats Provider 统计指标。
type ProviderStats struct {
	SuccessRate  float64
	P95Latency   float64
	CostAccuracy float64
	QualityScore float64
}

// SelectFallback 根据失败模式选择降级策略。
// 已被 inference.Classify + TierFromInt 替代，保留供旧调用方使用。
func SelectFallback(mode FailMode) FallbackTier {
	switch mode {
	case FailRateLimit:
		return FallbackSecondary
	case FailServerError:
		return FallbackSecondary
	case FailTimeout:
		return FallbackTertiary
	case FailContentFilter:
		return FallbackEscalate
	case FailTokenLimit:
		return FallbackTertiary
	default:
		return FallbackGraceful
	}
}

// TierFromInt 将 inference.ClassifiedError.FallbackTierInt() 的返回值转为 FallbackTier。
// 解耦两个包，调用方不需要同时 import substrate 和 substrate/inference。
//
// 用法：
//
//	ce := inference.ClassifyWithProvider(err, "anthropic")
//	tier := substrate.TierFromInt(ce.FallbackTierInt())
func TierFromInt(n int) FallbackTier {
	switch n {
	case 0:
		return FallbackPrimary
	case 1:
		return FallbackSecondary
	case 2:
		return FallbackTertiary
	case 3:
		return FallbackGraceful
	default:
		return FallbackEscalate
	}
}

// FallbackExecutor 执行降级链。
type FallbackExecutor struct {
	providers []Provider
	breaker   *CircuitBreaker
	scorer    *HealthScorer
	ctx       context.Context
}

// NewFallbackExecutor 创建。
func NewFallbackExecutor(ctx context.Context, p []Provider, b *CircuitBreaker, s *HealthScorer) *FallbackExecutor {
	return &FallbackExecutor{
		providers: p,
		breaker:   b,
		scorer:    s,
		ctx:       ctx,
	}
}

// Execute 按降级链顺序尝试每个 Provider，直到成功或全部失败。
// 流程: CircuitBreaker.Allow() → Provider.IsAvailable() → 执行（此处为可用性探测）
//
// 调用方通过注入 providers 列表控制降级顺序（Primary → Secondary → Tertiary）。
// 所有 Provider 均不可用时返回 ErrAllProvidersFailed。
func (fe *FallbackExecutor) Execute() error {
	if !fe.breaker.Allow() {
		return perrors.New(perrors.CodeInternal, "circuit breaker open — all providers throttled")
	}

	for i, p := range fe.providers {
		if !p.IsAvailable() {
			continue
		}
		// Provider 可用性探测成功，此处由调用方实现具体业务调用。
		// Execute 作为基础框架只做 Provider 选择与 CircuitBreaker 状态更新。
		fe.breaker.RecordResult(true)
		_ = i
		return nil
	}

	fe.breaker.RecordResult(false)
	return ErrAllProvidersFailed
}

// ErrAllProvidersFailed 全部 Provider 不可用时返回的哨兵错误。
var ErrAllProvidersFailed = perrors.New(perrors.CodeProviderExhausted, "fallback: all providers unavailable")

// Provider 降级链中的 Provider 引用。
type Provider interface {
	Name() string
	IsAvailable() bool
}
