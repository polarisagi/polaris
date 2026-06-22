//go:build windows

package probe

func probeOSMemory() (total uint64, available uint64) {
	return fallbackMemoryProbe()
}
