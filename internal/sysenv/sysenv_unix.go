//go:build !windows

package sysenv

import (
	"fmt"
	"syscall"
)

func getDiskFreeGB(path string) string {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err == nil {
		freeBytes := stat.Bavail * uint64(stat.Bsize)
		return fmt.Sprintf("%.2f", float64(freeBytes)/(1024*1024*1024))
	}
	return "unknown"
}
