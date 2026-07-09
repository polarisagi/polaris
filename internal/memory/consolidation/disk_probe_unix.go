//go:build !windows

package consolidation

import "syscall"

// diskFreeRatio 返回 path 所在文件系统的空闲空间占比（0~1）。
// ok=false 表示探测失败（如路径不存在、非常规文件系统），调用方应 fail-open
// （不阻断归档，视为"未知即不施加磁盘水位门控"，避免探针故障导致归档永久停摆）。
func diskFreeRatio(path string) (ratio float64, ok bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, false
	}
	if stat.Blocks == 0 {
		return 0, false
	}
	return float64(stat.Bavail) / float64(stat.Blocks), true
}
