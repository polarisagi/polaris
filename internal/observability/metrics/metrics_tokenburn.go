package metrics

// 2026-08-02 从 metrics.go 拆分（Test_inv_FileLineLimit R7 400 行上限存量债务，
// 见 local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现），纯搬运无行为变更。

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// TokenBurnRate tracks token consumption rate for circuit breaking.
// 架构文档: docs/arch/M03-Observability-深度选型.md §3
type TokenBurnRate struct {
	cumulativeTokens atomic.Int64
	lastTick         time.Time
	lastTokens       int64

	ema5s  float64
	ema30s float64

	baselineP95 float64
	callCount   atomic.Int64

	mu sync.RWMutex
}

func NewTokenBurnRate() *TokenBurnRate {
	return &TokenBurnRate{
		lastTick:    time.Now(),
		baselineP95: 200.0, // 冷启动保护值
	}
}

func (tbr *TokenBurnRate) Add(tokens int64) {
	tbr.cumulativeTokens.Add(tokens)
	tbr.callCount.Add(1)
}

// CumulativeTokens 返回累计 Add() 的 token 总量。阶段03 R-04 新增：Provider
// adapter 的 StreamInfer 转发协程测试需要直接断言"tbr 累加值正确"，EMA5s/
// EMA30s 依赖 Tick() 的时间窗口、不适合做确定性单测断言。
func (tbr *TokenBurnRate) CumulativeTokens() int64 {
	return tbr.cumulativeTokens.Load()
}

// Tick updates the EMA rates. Must be called periodically (e.g., every 1s).
func (tbr *TokenBurnRate) Tick() {
	tbr.mu.Lock()
	defer tbr.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tbr.lastTick).Seconds()
	if elapsed <= 0 {
		return
	}

	currentTokens := tbr.cumulativeTokens.Load()
	deltaTokens := currentTokens - tbr.lastTokens
	instantRate := float64(deltaTokens) / elapsed

	// α=0.33 for ~5s window
	tbr.ema5s = (0.33 * instantRate) + (1-0.33)*tbr.ema5s
	// α=0.06 for ~30s window
	tbr.ema30s = (0.06 * instantRate) + (1-0.06)*tbr.ema30s

	tbr.lastTokens = currentTokens
	tbr.lastTick = now

	// 动态基线学习：callCount >= 100 后启用 EWMA 更新 baselineP95。
	// α=0.05 对应约 20 次 Tick 平滑（1s 周期下约 20s 稳定），冷启动保留 200.0 兜底。
	if tbr.callCount.Load() >= 100 {
		if tbr.ema30s > tbr.baselineP95 {
			// 上行：快速跟随（防止误限速）
			tbr.baselineP95 = 0.2*tbr.ema30s + 0.8*tbr.baselineP95
		} else {
			// 下行：慢速收缩（防止因短时低谷使基线过低）
			tbr.baselineP95 = 0.02*tbr.ema30s + 0.98*tbr.baselineP95
		}
		// 下界保护：不低于 50 token/s（避免基线学成 0 导致永久限速）
		if tbr.baselineP95 < 50.0 {
			tbr.baselineP95 = 50.0
		}
	}
}

type ThrottleStage int

const (
	ThrottleNormal ThrottleStage = 0
	ThrottleStage1 ThrottleStage = 1 // THROTTLE
	ThrottleStage2 ThrottleStage = 2 // HARD STOP
	ThrottleStage3 ThrottleStage = 3 // FULLSTOP
)

// EMA5s 返回 5s 窗口 EMA 速率（token/s），供 /metrics 暴露。
func (tbr *TokenBurnRate) EMA5s() float64 {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()
	return tbr.ema5s
}

// EMA30s 返回 30s 窗口 EMA 速率（token/s），供 /metrics 暴露。
func (tbr *TokenBurnRate) EMA30s() float64 {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()
	return tbr.ema30s
}

// BaselineP95 返回动态学习的 P95 基线速率（token/s）。
// 供 ResourceBudget.BackgroundPermit 门控后台任务使用（C1.2）。
func (tbr *TokenBurnRate) BaselineP95() float64 {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()
	return tbr.baselineP95
}

func (tbr *TokenBurnRate) CheckThrottle() ThrottleStage {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()

	// 动态基线（callCount>=100 后由 Tick() EWMA 更新；冷启动兜底 200.0）
	limit := math.Max(tbr.baselineP95, 50.0)

	switch {
	case tbr.ema30s > limit*10.0:
		return ThrottleStage3
	case tbr.ema30s > limit*3.0:
		return ThrottleStage2
	case tbr.ema5s > limit*2.0:
		return ThrottleStage1
	default:
		return ThrottleNormal
	}
}
