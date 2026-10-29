package metrics

import (
	"math"
	"sync"
	"sync/atomic"
)

// GlobalCognitivePressure 全局认知压力单例（float64 bits 原子存储）。
// ADR-0001 observability 一等公民豁免，允许全局。
// 调用方用 GlobalCognitivePressure() 而非 GlobalCognitivePressure 直接访问实例。
var GlobalCognitivePressure = sync.OnceValue(func() *CognitivePressure { return &CognitivePressure{} })

// CognitivePressure 原子存储 float64（通过 IEEE 754 bit 转换，无锁）。
type CognitivePressure struct{ v atomic.Uint64 }

// Set 设置认知压力值（线程安全，无锁）。
func (c *CognitivePressure) Set(p float64) { c.v.Store(math.Float64bits(p)) }

// Current 读取当前认知压力值（线程安全，无锁）。
func (c *CognitivePressure) Current() float64 { return math.Float64frombits(c.v.Load()) }

// ComputeCognitivePressure 纯函数，用于 boot 周期压力 updater（C2.3）。
//
//   - active  = 活跃任务数（TaskClaimed + TaskExecuting 之和）
//   - surprise = GlobalSurpriseIndex().Current()（0–1 浮点）
//   - maxPrio  = 活跃任务最高优先级（0=最高..3=最低；无活跃任务传 3）
//
// 带边界保护防越界 panic。
func ComputeCognitivePressure(active int, surprise float64, maxPrio int) float64 {
	weights := [...]float64{1.0, 0.6, 0.3, 0.1}
	w := weights[len(weights)-1]
	if maxPrio >= 0 && maxPrio < len(weights) {
		w = weights[maxPrio]
	}
	return float64(active) * surprise * w
}
