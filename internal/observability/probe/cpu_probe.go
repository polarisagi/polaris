package probe

import (
	"sync"
	"time"
)

// CPU 占用率探针。按平台实现于 cpu_probe_{linux,darwin,windows,wasip1}.go：
//   - linux   /proc/stat 的 idle/total 增量（真实占用率）
//   - darwin  sysctl vm.loadavg 的 1 分钟负载 ÷ 逻辑核数（压力代理值，见该文件注释）
//   - windows GetSystemTimes 的 idle/kernel/user 增量（真实占用率）
//
// 替代原 automation 私有 cpuSampler：它在非 Linux 平台以 goroutine 数量估算，恒返回 80.0。

// CPUSampler 带 1 秒缓存的 CPU 占用率采样器。
//
// 增量型实现需要保存上一次快照才能算出增量，因此采样器本身是有状态的；由调用方
// 持有实例而非放包级单例，既满足 R1.3（禁止全局可变变量），也让测试能各自持有
// 独立采样器互不干扰。零值可直接使用。
type CPUSampler struct {
	mu       sync.Mutex
	state    cpuSampleState
	lastTime time.Time
	lastPct  float64
}

// NewCPUSampler 创建一个 CPU 占用率采样器。
func NewCPUSampler() *CPUSampler { return &CPUSampler{} }

// Usage 返回系统 CPU 占用率百分比（0–100）。
// 首次调用因无上一次快照可能返回 0，后续调用给出增量真实值。
// 采样结果缓存 1 秒：既避免高频 syscall，也保证增量窗口不至于短到失真。
func (cs *CPUSampler) Usage() float64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if !cs.lastTime.IsZero() && time.Since(cs.lastTime) < time.Second {
		return cs.lastPct
	}
	pct, st, ok := sampleCPU(cs.state)
	cs.lastTime = time.Now()
	if !ok {
		return cs.lastPct
	}
	cs.state = st
	cs.lastPct = clampPercent(pct)
	return cs.lastPct
}

func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}
