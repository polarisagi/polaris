//go:build windows

package probe

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processMemoryCounters 对应 Win32 PROCESS_MEMORY_COUNTERS（psapi.h）。
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// LazyDLL/LazyProc 只读惰性绑定，理由同 memory_probe_windows.go。
//
//nolint:gochecknoglobals // 理由见上
var (
	psapiDLL                 = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapiDLL.NewProc("GetProcessMemoryInfo")
)

// processPeakRSSBytes 返回本进程的峰值工作集（等价于 Linux 的 VmHWM）。
//
// 不能用 runtime.MemStats.Sys：它只含 Go 运行时保留的地址空间、不含 FFI 侧且不是
// 峰值，M11 §5.3 本地模型内存守卫据此会错误放行。LazyDLL 绑定 psapi 为纯 Go。
func processPeakRSSBytes() uint64 {
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	r1, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(windows.CurrentProcess()),
		uintptr(unsafe.Pointer(&pmc)),
		uintptr(pmc.CB),
	)
	if r1 == 0 || pmc.PeakWorkingSetSize == 0 {
		// 保守兜底：返回 Go 运行时保留量而非 0——调用方拿到 0 会误判为
		// "零内存占用"从而错误放行 Tier3 内存守卫检查。
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.Sys
	}
	return uint64(pmc.PeakWorkingSetSize)
}
