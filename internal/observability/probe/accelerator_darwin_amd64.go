//go:build darwin && amd64

package probe

import (
	"context"
	"encoding/binary"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

func probePlatformAccelerator(ctx context.Context) AcceleratorInfo {
	brand, _ := unix.Sysctl("machdep.cpu.brand_string")
	if brand == "" {
		brand = "Intel Core"
	}

	totalRAM, _ := MemoryProbe()
	memTotalMB := totalRAM / (1024 * 1024)

	physCPUs, _ := unix.SysctlUint32("hw.physicalcpu")
	if physCPUs == 0 {
		physCPUs = 4
	}

	load1 := float64(0)
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		l := binary.LittleEndian.Uint32(b[0:4])
		fscale := binary.LittleEndian.Uint64(b[16:24])
		if fscale > 0 {
			load1 = float64(l) / float64(fscale)
		}
	}

	// Intel Mac 一律 Accelerated = false（ADR-0109 P3）
	// 可记录 system_profiler SPDisplaysDataType 的 GPU 名称供展示
	model := brand
	out, err := exec.CommandContext(ctx, "system_profiler", "SPDisplaysDataType").Output()
	if err == nil {
		if gpu := parseDarwinDisplaysOutput(string(out)); gpu != "" {
			model = gpu
		}
	}

	return AcceleratorInfo{
		Kind:         "none",
		Model:        strings.TrimSpace(model),
		MemoryMB:     memTotalMB,
		Accelerated:  false,
		PhysicalCPUs: int(physCPUs),
		Load1Min:     load1,
	}
}
