package llm

import (
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/config"
)

// circuitState 熔断器状态。
type circuitState int32

const (
	circuitClosed   circuitState = iota // 正常放行
	circuitOpen                         // 拒绝请求
	circuitHalfOpen                     // 探测恢复
)

// circuitBreaker 连续失败 → Open(冷却期) → HalfOpen 探测。
// 架构文档: M01 §4.5（参数权威源 spec/state.yaml §m1_router.circuit_breaker_*）
type circuitBreaker struct {
	state       atomic.Int32
	failures    atomic.Int32
	openUntil   atomic.Int64 // unix nano
	maxFailures int32
	openDur     time.Duration
}

// newCircuitBreaker 按 M1RouterThresholds 配置创建熔断器。
// 零值字段回退 spec/state.yaml 默认值（5 次失败 / 10s 冷却）。
func newCircuitBreaker(cfg config.M1RouterThresholds) *circuitBreaker {
	maxFail := int32(cfg.CircuitBreakerFailureCount)
	if maxFail <= 0 {
		maxFail = 5
	}
	cooldown := time.Duration(cfg.CircuitBreakerCooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	cb := &circuitBreaker{maxFailures: maxFail, openDur: cooldown}
	cb.state.Store(int32(circuitClosed))
	return cb
}

func (cb *circuitBreaker) Allow() bool {
	switch circuitState(cb.state.Load()) {
	case circuitClosed:
		return true
	case circuitOpen:
		if time.Now().UnixNano() > cb.openUntil.Load() {
			cb.state.Store(int32(circuitHalfOpen))
			return true
		}
		return false
	case circuitHalfOpen:
		return true
	}
	return false
}

func (cb *circuitBreaker) RecordSuccess() (recovered bool) {
	prev := circuitState(cb.state.Load())
	cb.failures.Store(0)
	cb.state.Store(int32(circuitClosed))
	return prev == circuitHalfOpen
}

func (cb *circuitBreaker) RecordFailure() {
	n := cb.failures.Add(1)
	if n >= cb.maxFailures {
		cb.state.Store(int32(circuitOpen))
		cb.openUntil.Store(time.Now().Add(cb.openDur).UnixNano())
		cb.failures.Store(0)
	}
}
