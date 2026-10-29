//go:build darwin

package probe

import (
	"encoding/binary"
	"runtime"

	"golang.org/x/sys/unix"
)

// cpuSampleState 在 darwin 上不需要跨次快照（负载均值本身就是内核维护的滑动窗口）。
type cpuSampleState struct{}

// sampleCPU 用 sysctl vm.loadavg 的 1 分钟负载均值除以逻辑核数，估算系统 CPU 压力。
//
// 精确占用率需要 mach host_processor_info / host_statistics(HOST_CPU_LOAD_INFO)，
// 二者都只有 mach trap 入口，纯 Go（CGO_ENABLED=0，见 scripts/restart.sh）拿不到；
// 唯一免 CGO 的替代是 fork 出 /usr/bin/top 之类解析输出——准入路径上每次判定都
// 起一个子进程，代价远大于收益。负载均值是内核维护的可运行线程数滑动平均，
// 作为"系统忙不忙"的压力代理足够，且语义上比原先的"goroutine 数量启发式"
// （恒返回 80.0）强一个量级。
//
// 注意其与真实占用率的差异：负载均值统计的是可运行 + 不可中断等待的线程数，
// 因此磁盘 IO 阻塞也会抬高它；满载时可以超过 100%，由上层 clampPercent 截断。
//
// struct loadavg（sys/sysctl.h）：{ fixpt_t ldavg[3]; long fscale; }
// arm64/amd64 下布局为 3×uint32（12B）+ 4B 对齐填充 + 8B fscale = 24B。
func sampleCPU(prev cpuSampleState) (pct float64, next cpuSampleState, ok bool) {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(b) < 24 {
		return 0, prev, false
	}
	load1 := binary.LittleEndian.Uint32(b[0:4])
	fscale := binary.LittleEndian.Uint64(b[16:24])
	if fscale == 0 {
		return 0, prev, false
	}
	cores := runtime.NumCPU()
	if cores <= 0 {
		return 0, prev, false
	}
	return float64(load1) / float64(fscale) / float64(cores) * 100, prev, true
}
