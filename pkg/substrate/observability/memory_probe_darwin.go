//go:build darwin

package observability

import (
	"bytes"
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// vmtotalDarwin mirrors struct vmtotal from XNU sys/resource.h.
// Layout: 5×int16 (10B) + 2B alignment pad + 9×int32 (36B) = 48B.
type vmtotalDarwin struct {
	Trq, Tdw, Tpw, Tsl, Tsw                 int16
	Pad                                     int16 // alignment pad before int32 fields
	Tvm, Tavm, Trm, Tarm                    int32
	Tvmshr, Tavmshr, Trmshr, Tarmshr, Tfree int32
}

func probeOSMemory() (total uint64, available uint64) {
	totalBytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil || totalBytes == 0 {
		return fallbackMemoryProbe()
	}
	total = totalBytes
	pageSize := uint64(unix.Getpagesize())

	// vm.vmtotal gives page-level stats: T_free (truly free) and T_rm/T_arm (active+inactive).
	// available ≈ T_free + 50% of inactive pages (T_rm - T_arm).
	b, err := unix.SysctlRaw("vm.vmtotal")
	if err == nil && len(b) >= int(unsafe.Sizeof(vmtotalDarwin{})) { //nolint:nestif
		var vt vmtotalDarwin
		if binary.Read(bytes.NewReader(b), binary.LittleEndian, &vt) == nil {
			freePg := int64(vt.Tfree)
			inactive := int64(vt.Trm - vt.Tarm)
			if inactive < 0 {
				inactive = 0
			}
			availPg := freePg + inactive/2
			if availPg > 0 {
				available = uint64(availPg) * pageSize
				if available < 2*1024*1024*1024 {
					available = 2 * 1024 * 1024 * 1024
				}
				return total, available
			}
		}
	}

	// Fallback: conservative 40% when vm.vmtotal unavailable.
	available = total * 40 / 100
	if available < 2*1024*1024*1024 {
		available = 2 * 1024 * 1024 * 1024
	}
	return total, available
}
