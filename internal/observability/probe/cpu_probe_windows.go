//go:build windows

package probe

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

//nolint:gochecknoglobals // 同 memory_probe_windows.go：LazyProc 只读惰性绑定。
var procGetSystemTimes = kernel32DLL.NewProc("GetSystemTimes")

// cpuSampleState 保存上一次 GetSystemTimes 快照（100ns 单位累计值）。
type cpuSampleState struct {
	idle  uint64
	total uint64
}

func filetimeToUint64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// sampleCPU 用 kernel32!GetSystemTimes 的 idle/kernel/user 累计值增量算真实占用率。
// 注意 kernel 时间**已包含** idle 时间，故 total = kernel + user。
// golang.org/x/sys/windows 未导出该调用，经 LazyProc 绑定（纯 Go，无 CGO）。
func sampleCPU(prev cpuSampleState) (pct float64, next cpuSampleState, ok bool) {
	var idleFT, kernelFT, userFT windows.Filetime
	r1, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernelFT)),
		uintptr(unsafe.Pointer(&userFT)),
	)
	if r1 == 0 {
		return 0, prev, false
	}

	cur := cpuSampleState{
		idle:  filetimeToUint64(idleFT),
		total: filetimeToUint64(kernelFT) + filetimeToUint64(userFT),
	}
	if prev.total == 0 || cur.total <= prev.total {
		return 0, cur, false
	}
	dTotal := cur.total - prev.total
	dIdle := cur.idle - prev.idle
	return float64(dTotal-dIdle) / float64(dTotal) * 100, cur, true
}
