//go:build windows

package probe

import (
	"context"
	"os/exec"
	"runtime"
)

func probePlatformAccelerator(ctx context.Context) AcceleratorInfo {
	physCPUs := runtime.NumCPU()
	load1 := float64(0)

	out, err := exec.CommandContext(ctx, "nvidia-smi.exe", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return AcceleratorInfo{
			Kind:         "none",
			Model:        "",
			MemoryMB:     0,
			Accelerated:  false,
			PhysicalCPUs: physCPUs,
			Load1Min:     load1,
		}
	}

	info, parseErr := parseNvidiaSmiOutput(string(out))
	if parseErr != nil {
		return AcceleratorInfo{
			Kind:         "none",
			PhysicalCPUs: physCPUs,
			Load1Min:     load1,
		}
	}
	info.PhysicalCPUs = physCPUs
	info.Load1Min = load1
	return info
}
