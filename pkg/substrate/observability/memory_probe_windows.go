//go:build windows

package observability

func probeOSMemory() (total uint64, available uint64) {
	return fallbackMemoryProbe()
}
