//go:build linux

package probe

import "golang.org/x/sys/unix"

// processPeakRSSBytes 通过 getrusage(RUSAGE_SELF) 读取 ru_maxrss。
// Linux 上 ru_maxrss 单位是 KB，需换算为字节。
func processPeakRSSBytes() uint64 {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if ru.Maxrss < 0 {
		return 0
	}
	return uint64(ru.Maxrss) * 1024
}
