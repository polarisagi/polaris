//go:build darwin

package probe

import (
	"golang.org/x/sys/unix"
)

// probeOSMemory 读取 macOS 系统内存总量与可用量。
//
// [2026-09-22 重写] 原实现基于 sysctl "vm.vmtotal" 解析 struct vmtotal。该
// sysctl 在现代 macOS 上**根本不存在**（实测 Darwin 27 返回 ENOENT），于是每次
// 都落到最后那条 `available = total * 40 / 100` 的盲猜兜底上——本机 16GB 实际
// 可用 2.5GB 时它恒报 6553MB。ResourceGovernor 拿这个数去比 mem_l2_free_mb
// 阈值做准入判定，等于在拿一个常数做决策。原实现还有一条
// `if available < 2GB { available = 2GB }` 的下限钳制：内存真的只剩 300MB 时
// 它也报 2GB，把内存压力检测彻底失效掉，故一并删除——探针的职责是如实上报，
// "太低了怎么办"是调用方的策略，不能在探针里粉饰。
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
