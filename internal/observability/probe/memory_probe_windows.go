//go:build windows

package probe

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx 对应 Win32 MEMORYSTATUSEX（winbase.h）。
// golang.org/x/sys/windows 未导出该结构与 GlobalMemoryStatusEx，故在此自行声明
// 并经 LazyDLL 调用——纯 Go，不引入 CGO（构建走 CGO_ENABLED=0）。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// LazyDLL/LazyProc 是 Windows 系统调用的标准惰性绑定形式：进程内唯一且只读
// （内部自带 sync.Once），与 ADR-0001 禁止的可变全局状态无关。
//
//nolint:gochecknoglobals // 理由见上
var (
	kernel32DLL              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32DLL.NewProc("GlobalMemoryStatusEx")
)

// probeOSMemory 读取 Windows 物理内存总量与可用量。
//
// 经 LazyDLL 绑定 kernel32（纯 Go，免 CGO）；失败才退回 fallbackMemoryProbe 的保守估计。
func probeOSMemory() (total uint64, available uint64) {
	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	// GlobalMemoryStatusEx 成功返回非零；失败时 r1==0，退回保守兜底。
	r1, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r1 == 0 || st.TotalPhys == 0 {
		return fallbackMemoryProbe()
	}
	// AvailPhys = 立即可分配的物理内存（含待机页），语义与 Linux MemAvailable 对齐。
	return st.TotalPhys, st.AvailPhys
}
