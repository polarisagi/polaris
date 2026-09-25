//go:build darwin

package probe

import (
	"golang.org/x/sys/unix"
)

// probeOSMemory 读取 macOS 系统内存总量与可用量。
//
// 不用 vm.vmtotal（现代 macOS 已无此 sysctl，旧实现恒落到 total×40% 盲猜）；不做
// 下限钳制——探针如实上报，低内存如何处置是调用方的策略。
func probeOSMemory() (total uint64, available uint64) {
	totalBytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil || totalBytes == 0 {
		return fallbackMemoryProbe()
	}
	total = totalBytes

	// 首选 kern.memorystatus_level：XNU 自己维护的"系统可用内存百分比"，
	// 与 /usr/bin/memory_pressure 输出的 "System-wide memory free percentage"
	// 同源，且是 macOS 自身内存压力通知的判定依据。它计入了可回收页
	// （inactive / purgeable / 可压缩），比"纯空闲页"更贴近"还能拿来用多少"。
	if lvl, lvlErr := unix.SysctlUint32("kern.memorystatus_level"); lvlErr == nil && lvl > 0 && lvl <= 100 {
		return total, total * uint64(lvl) / 100
	}

	// 退路：vm.page_free_count 纯空闲页数。偏保守（不含可回收页），但真实。
	if freePg, pgErr := unix.SysctlUint32("vm.page_free_count"); pgErr == nil {
		return total, uint64(freePg) * uint64(unix.Getpagesize())
	}

	return fallbackMemoryProbe()
}
