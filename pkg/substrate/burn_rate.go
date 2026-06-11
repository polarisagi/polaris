package substrate

import "time"

// TokenBurnRate — 熔断信号计算（M3 权威实现）。
// 架构文档: docs/arch/03-Observability-深度选型.md §3
// 双窗 EMA: EMA_5s(α=0.33) + EMA_30s(α=0.06)

type BurnRateTracker struct {
	ema5s       float64
	ema30s      float64
	baselineP95 float64
	lastTick    time.Time
}

func (b *BurnRateTracker) Add(tokens int, now time.Time) {
	delta := now.Sub(b.lastTick).Seconds()
	if delta < 0.001 {
		return
	}
	instantRate := float64(tokens) / delta

	alpha5s := 0.33
	alpha30s := 0.06

	b.ema5s = alpha5s*instantRate + (1-alpha5s)*b.ema5s
	b.ema30s = alpha30s*instantRate + (1-alpha30s)*b.ema30s
	b.lastTick = now
}

// Stage 返回当前熔断阶段: 0=Normal, 1=THROTTLE, 2=HARD_STOP.
// baselineP95 <= 0 表示基线尚未建立（冷启动），始终返回 Normal，
// 避免零基线导致任何正 EMA 值都触发 HARD_STOP 的误熔断。
func (b *BurnRateTracker) Stage() int {
	if b.baselineP95 <= 0 {
		return 0
	}
	if b.ema30s > b.baselineP95*3.0 {
		return 2
	}
	if b.ema5s > b.baselineP95*2.0 {
		return 1
	}
	return 0
}

// SetBaseline 设置 P95 基线（需在足够样本积累后调用，通常为启动后 5 分钟）。
func (b *BurnRateTracker) SetBaseline(p95 float64) {
	b.baselineP95 = p95
}
