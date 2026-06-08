//go:build linux

package observability

import (
	"golang.org/x/sys/unix"
)

func probeOSMemory() (total uint64, available uint64) {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return fallbackMemoryProbe()
	}
	total = si.Totalram * uint64(si.Unit)
	free := si.Freeram * uint64(si.Unit)
	buffers := si.Bufferram * uint64(si.Unit)
	available = free + buffers
	if total == 0 {
		return fallbackMemoryProbe()
	}
	return total, available
}
