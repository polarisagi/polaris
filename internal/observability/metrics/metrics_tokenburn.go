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
// 架构文档: docs/arch/M03-Observability.md §3
type TokenBurnRate struct {
	cumulativeTokens atomic.Int64
	lastTick         time.Time
	lastTokens       int64

	ema5s  float64
	ema30s float64

	baselineP95 float64
	// throttleFloor 是 Stage1 判据的基线底线（tok/s），与 baselineP95 分离：
	// baselineP95 还被 BackgroundPermit 用作"典型负载"来限制后台任务，不能随熔断校准一起抬高。
	throttleFloor float64
	callCount     atomic.Int64

	mu sync.RWMutex
}

func NewTokenBurnRate() *TokenBurnRate {
	return NewTokenBurnRateAt(time.Now())
}

// NewTokenBurnRateAt 以指定起点构造，供 Eval 重放历史时间轴（与 TickAt 配套）。
func NewTokenBurnRateAt(start time.Time) *TokenBurnRate {
	return &TokenBurnRate{
		lastTick:      start,
		baselineP95:   200.0, // 冷启动保护值（仅用于后台任务门控，见 throttleFloor）
		throttleFloor: DefaultThrottleBaseline,
	}
}

const (
	// DefaultThrottleBaseline Stage1 判据基线底线的默认值（tok/s）。
	// 由 Eval 选定，数据见 internal/eval/harness/burnrate_eval_test.go 与 state.yaml token_burn_rate。
	DefaultThrottleBaseline = 4000.0
	// ThrottleMultiplier Stage1 触发倍数（state.yaml：TokenBurnRate > 2x baseline）。
	ThrottleMultiplier = 2.0
)

// SetThrottleBaseline 覆盖 Stage1 判据的基线底线（tok/s）；非正值忽略。启动期由配置注入。
func (tbr *TokenBurnRate) SetThrottleBaseline(v float64) {
	if v <= 0 {
		return
	}
	tbr.mu.Lock()
	tbr.throttleFloor = v
	tbr.mu.Unlock()
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
	tbr.TickAt(time.Now())
}

// TickAt 以显式时刻推进 EMA。拆出是为了让 Eval 能按真实调用时间轴确定性重放，而不依赖墙钟睡眠。
func (tbr *TokenBurnRate) TickAt(now time.Time) {
	tbr.mu.Lock()
	defer tbr.mu.Unlock()

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
	// 越过 Stage1 触发线的速率不参与学习：否则持续失控会把基线一路抬到失控速率本身，
	// 触发线随之追着跑，失控反而"变正常"（learned baseline 追逐 bug）。
	over := tbr.ema30s > ThrottleMultiplier*math.Max(tbr.baselineP95, tbr.throttleFloor)
	if tbr.callCount.Load() >= 100 && !over {
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

// OverThrottleLine 报告当前燃烧率是否越过 KillSwitch Stage1 触发线（state.yaml kill_switch.Stage1_THROTTLE）。
//
// 判据用 EMA_30s 而非 EMA_5s：调用按 chunk/整次响应批量入账，单次大上下文调用在 1s 窗口内
// 可瞬时折算出数万 tok/s，EMA_5s 对正常会话也会频繁越线（Eval 重放见 harness/burnrate_eval_test.go）；
// EMA_30s 只对持续失控敏感。
// 基线取 max(学习值, 校准底线)：学习值只会抬高触发线，不会因均值偏低把正常波动判为失控。
func (tbr *TokenBurnRate) OverThrottleLine() bool {
	tbr.mu.RLock()
	defer tbr.mu.RUnlock()
	return tbr.ema30s > ThrottleMultiplier*math.Max(tbr.baselineP95, tbr.throttleFloor)
}
