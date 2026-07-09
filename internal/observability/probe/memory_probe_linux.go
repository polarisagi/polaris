//go:build linux

package probe

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func probeOSMemory() (total uint64, available uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		// Fallback to sysinfo
		var si unix.Sysinfo_t
		if err := unix.Sysinfo(&si); err == nil {
			total = si.Totalram * uint64(si.Unit)
			available = (si.Freeram + si.Bufferram) * uint64(si.Unit)
			if total > 0 {
				return total, available
			}
		}
		return fallbackMemoryProbe()
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > 13 && line[:13] == "MemAvailable:" {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, _ := strconv.ParseUint(fields[1], 10, 64)
				available = val * 1024 // kB to bytes
			}
		} else if len(line) > 9 && line[:9] == "MemTotal:" {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				val, _ := strconv.ParseUint(fields[1], 10, 64)
				total = val * 1024 // kB to bytes
			}
		}
	}
	if total == 0 {
		return fallbackMemoryProbe()
	}
	return total, available
}
