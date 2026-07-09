//go:build windows

package probe

import "runtime"

// processPeakRSSBytes Windows 无 getrusage 等价 API（GetProcessMemoryInfo 需 CGO/
// syscall 绑定 psapi.dll，超出本次范围）；返回 runtime.MemStats.Sys 作为保守估计，
// 避免调用方因返回 0 而误判"零内存占用"从而错误放行 Tier3 内存守卫检查。
func processPeakRSSBytes() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}
