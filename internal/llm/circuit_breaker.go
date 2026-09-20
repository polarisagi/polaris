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
	probing     atomic.Bool  // 保障单探针语义 (WP-7)
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

// Available 无副作用地判断是否可被选为候选（不获取 HalfOpen 探测权）。
//
// 选路必须用它而非 Allow（2026-09-19，GR-2.2-002 复核时发现）：选路循环对每个候选都调
// Allow，冷却期满的 entry 会在"只是被比较、未被选中"时就 CAS 成 HalfOpen 并占住唯一探测权；
// 之后它既不会被发请求、也就不会 RecordSuccess/Failure，probing 永不释放 → 该 Provider
// 在进程生命周期内被永久排除。ModelID()/Tokenizer()/PickProviderName() 等只读调用同样会触发。
func (cb *circuitBreaker) Available() bool {
	switch circuitState(cb.state.Load()) {
	case circuitClosed:
		return true
	case circuitOpen:
		return time.Now().UnixNano() > cb.openUntil.Load()
	case circuitHalfOpen:
		return !cb.probing.Load()
	}
	return false
}

// Allow 获取放行（HalfOpen 下即获取唯一探测权）。只应对**最终选中并即将发请求**的 entry 调用。
func (cb *circuitBreaker) Allow() bool {
	switch circuitState(cb.state.Load()) {
	case circuitClosed:
		return true
	case circuitOpen:
		if time.Now().UnixNano() > cb.openUntil.Load() {
			if cb.state.CompareAndSwap(int32(circuitOpen), int32(circuitHalfOpen)) {
				cb.probing.Store(true)
				return true // 仅本次 CAS 成功的 goroutine 获得探测权
			}
			return false // 其余并发请求视为仍处于 Open
		}
		return false
	case circuitHalfOpen:
		return cb.probing.CompareAndSwap(false, true)
	}
	return false
}

func (cb *circuitBreaker) RecordSuccess() (recovered bool) {
	prev := circuitState(cb.state.Load())
	cb.failures.Store(0)
	cb.state.Store(int32(circuitClosed))
	if prev == circuitHalfOpen {
		cb.probing.Store(false)
		return true
	}
	return false
}

func (cb *circuitBreaker) RecordFailure() {
	if circuitState(cb.state.Load()) == circuitHalfOpen {
		cb.state.Store(int32(circuitOpen))
		cb.openUntil.Store(time.Now().Add(cb.openDur).UnixNano())
		cb.failures.Store(0)
		cb.probing.Store(false)
		return
	}

	n := cb.failures.Add(1)
	if n >= cb.maxFailures {
		cb.state.Store(int32(circuitOpen))
		cb.openUntil.Store(time.Now().Add(cb.openDur).UnixNano())
		cb.failures.Store(0)
	}
}
