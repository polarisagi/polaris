//go:build darwin && arm64

package probe

import (
	"context"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

func probePlatformAccelerator(_ context.Context) AcceleratorInfo {
	brand, _ := unix.Sysctl("machdep.cpu.brand_string")
	if brand == "" {
		brand = "Apple Silicon"
	}

	totalRAM, _ := MemoryProbe()
	memTotalMB := totalRAM / (1024 * 1024)

	physCPUs, _ := unix.SysctlUint32("hw.physicalcpu")
	if physCPUs == 0 {
		physCPUs = 8
	}

	load1 := float64(0)
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		l := binary.LittleEndian.Uint32(b[0:4])
		fscale := binary.LittleEndian.Uint64(b[16:24])
		if fscale > 0 {
			load1 = float64(l) / float64(fscale)
		}
	}

	// 统一内存 >= 16GB (16384 MB) 视为具备本地大模型加速能力（ADR-0109 P3）
	accelerated := memTotalMB >= 16384

	return AcceleratorInfo{
		Kind:         "apple_silicon",
		Model:        brand,
		MemoryMB:     memTotalMB,
		Accelerated:  accelerated,
		PhysicalCPUs: int(physCPUs),
		Load1Min:     load1,
	}
}
