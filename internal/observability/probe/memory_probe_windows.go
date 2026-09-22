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
// [2026-09-22 实现] 此前本文件直接 `return fallbackMemoryProbe()`——那条兜底会
// 硬编码 "假设总内存 8GB" 并用 `m.Sys - m.HeapAlloc`（Go 运行时自身的 arena 会计，
// 与系统内存无关）充当可用量。于是 Windows 上的 Tier 分级、FeatureGate 硬件门控、
// ResourceGovernor 准入判定全部建立在两个虚构数字上。
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
