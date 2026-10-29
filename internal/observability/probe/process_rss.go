package probe

// ProcessPeakRSSBytes 返回当前进程峰值常驻内存（Peak/HWM RSS，单位字节）。
// 平台实现见 process_rss_linux.go / process_rss_darwin.go / process_rss_windows.go。
// 用途: M11 local_only Tier3 内存守卫（docs/arch/M11-Policy-Safety.md §5.3），
// 在尝试加载本地模型前后度量进程峰值内存占用，与系统已用内存共同决定是否
// 拒绝进入 local_only 模式（fail-closed）。
func ProcessPeakRSSBytes() uint64 {
	return processPeakRSSBytes()
}
