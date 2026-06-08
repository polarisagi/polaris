package observability

import (
	"runtime"
)

// memoryProbe returns total and available system RAM in bytes.
// Platform-specific implementations are in memory_probe_linux.go / memory_probe_darwin.go.
func memoryProbe() (total uint64, available uint64) {
	return probeOSMemory()
}

// ProbeAvailableMemoryMB returns the current estimate of free + reclaimable RAM in MiB.
// Used by the runtime memory pressure monitor started in main.
func ProbeAvailableMemoryMB() uint64 {
	_, available := probeOSMemory()
	return available / (1024 * 1024)
}

// fallbackMemoryProbe returns a conservative estimate when OS probing fails.
func fallbackMemoryProbe() (total uint64, available uint64) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// Conservative estimate: assume 8GB total, runtime-managed as available indicator
	total = 8 * 1024 * 1024 * 1024
	heapMB := m.HeapAlloc / (1024 * 1024)
	sysMB := m.Sys / (1024 * 1024)
	if sysMB > heapMB {
		available = (sysMB - heapMB) * 1024 * 1024
	} else {
		available = 2 * 1024 * 1024 * 1024 // assume 2GB free
	}
	if available > total {
		available = total / 2
	}
	return total, available
}
