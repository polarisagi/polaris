//go:build windows

package consolidation

import "golang.org/x/sys/windows"

// diskFreeRatio 返回 path 所在卷的空闲空间占比（0~1）。
// ok=false 表示探测失败，调用方应 fail-open（不阻断归档）。
func diskFreeRatio(path string) (ratio float64, ok bool) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var freeBytes, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeBytes, &totalBytes, &totalFreeBytes); err != nil {
		return 0, false
	}
	if totalBytes == 0 {
		return 0, false
	}
	return float64(freeBytes) / float64(totalBytes), true
}
