//go:build !((darwin && (arm64 || amd64)) || linux || windows)

package probe

import (
	"context"
	"runtime"
)

func probePlatformAccelerator(_ context.Context) AcceleratorInfo {
	return AcceleratorInfo{
		Kind:         "none",
		Model:        "generic",
		MemoryMB:     0,
		Accelerated:  false,
		PhysicalCPUs: runtime.NumCPU(),
		Load1Min:     0,
	}
}
