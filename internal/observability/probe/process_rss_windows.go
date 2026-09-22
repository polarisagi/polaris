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
// [2026-09-22 实现] 此前本文件返回 runtime.MemStats.Sys，注释写明
// "GetProcessMemoryInfo 需 CGO/syscall 绑定 psapi.dll，超出本次范围"。实际上
// LazyDLL 绑定是纯 Go 的（CGO_ENABLED=0 照常可用），而 MemStats.Sys 只反映 Go
// 运行时自己保留的地址空间，既不含 cgo/FFI 侧（Rust substrate dylib）的占用，
// 也不是峰值——M11 §5.3 Tier3 本地模型内存守卫正是靠这个值判断"再加载一个模型
// 会不会把机器打爆"，读数偏低会让守卫错误放行。
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
