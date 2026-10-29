//go:build darwin

package probe

import "golang.org/x/sys/unix"

// processPeakRSSBytes 通过 getrusage(RUSAGE_SELF) 读取 ru_maxrss。
// Darwin 上 ru_maxrss 单位已是字节（与 Linux 的 KB 语义不同）。
func processPeakRSSBytes() uint64 {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if ru.Maxrss < 0 {
		return 0
	}
	return uint64(ru.Maxrss)
}
