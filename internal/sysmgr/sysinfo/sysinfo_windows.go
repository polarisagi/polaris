//go:build windows

package sysinfo

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func getDiskFreeGB(path string) string {
	var freeBytes, totalBytes, totalFreeBytes uint64
	p := path
	if p == "/" {
		p = `C:\`
	}
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "unknown"
	}
	err = windows.GetDiskFreeSpaceEx(ptr, &freeBytes, &totalBytes, &totalFreeBytes)
	if err == nil {
		return fmt.Sprintf("%.2f", float64(freeBytes)/(1024*1024*1024))
	}
	return "unknown"
}
