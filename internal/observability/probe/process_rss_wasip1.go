//go:build wasip1

package probe

import "runtime"

func processPeakRSSBytes() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}
