package llm

import (
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

// windowCircuitBreaker 基于滑动时间窗口的错误率熔断器。
// 补充现有 circuitBreaker（连续失败计数），两者 OR 逻辑：任一触发则熔断。
// 参数权威源：spec/state.yaml §m1_router.window_breaker_*
type windowCircuitBreaker struct {
	windowSecs int64
	minSamples int64
	threshold  float64
	cooldown   time.Duration

	windowStart atomic.Int64
	successes   atomic.Int64
	failures    atomic.Int64
	openUntil   atomic.Int64 // unix nano；0 = 未熔断
}

func newWindowCircuitBreaker(cfg config.M1RouterThresholds) *windowCircuitBreaker {
	w := cfg.WindowBreakerWindowSecs
	if w <= 0 {
		w = 60
	}
	m := cfg.WindowBreakerMinSamples
	if m <= 0 {
		m = 20
	}
	t := cfg.WindowBreakerThreshold
	if t <= 0 {
		t = 0.5
	}
	c := time.Duration(cfg.WindowBreakerCooldownSec) * time.Second
	if c <= 0 {
		c = 30 * time.Second
	}
	wcb := &windowCircuitBreaker{
		windowSecs: int64(w),
		minSamples: int64(m),
		threshold:  t,
		cooldown:   c,
	}
	wcb.windowStart.Store(time.Now().Unix())
	return wcb
}

// maybeResetWindow 检查当前窗口是否过期，若是则原子重置计数。
func (w *windowCircuitBreaker) maybeResetWindow() {
	now := time.Now().Unix()
	start := w.windowStart.Load()
	if now-start >= w.windowSecs {
		// CAS 保证只有一个 goroutine 执行重置
		if w.windowStart.CompareAndSwap(start, now) {
			w.successes.Store(0)
			w.failures.Store(0)
		}
	}
}

// Allow 返回 false 时表示熔断触发，拒绝请求。
func (w *windowCircuitBreaker) Allow() bool {
	if w.openUntil.Load() > time.Now().UnixNano() {
		return false // 冷却中
	}
	w.maybeResetWindow()
	return true
}

// RecordSuccess 记录成功请求。
func (w *windowCircuitBreaker) RecordSuccess() {
	w.maybeResetWindow()
	w.successes.Add(1)
}

// RecordFailure 记录失败请求，若错误率超阈值则触发熔断。
func (w *windowCircuitBreaker) RecordFailure() {
	w.maybeResetWindow()
	f := w.failures.Add(1)
	s := w.successes.Load()
	total := f + s
	if total >= w.minSamples {
		rate := float64(f) / float64(total)
		if rate >= w.threshold {
			until := time.Now().Add(w.cooldown).UnixNano()
			w.openUntil.Store(until)
		}
	}
}
