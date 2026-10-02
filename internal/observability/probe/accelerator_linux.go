//go:build linux

package probe

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func probePlatformAccelerator(ctx context.Context) AcceleratorInfo {
	physCPUs := linuxPhysicalCPUs()
	load1 := linuxLoad1()

	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
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

func linuxPhysicalCPUs() int {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return runtime.NumCPU()
	}
	defer f.Close()

	cores := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	var physID, coreID string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "physical id") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				physID = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "core id") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				coreID = strings.TrimSpace(parts[1])
				cores[physID+":"+coreID] = true
			}
		}
	}
	if len(cores) > 0 {
		return len(cores)
	}
	return runtime.NumCPU()
}

func linuxLoad1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) > 0 {
		if val, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return val
		}
	}
	return 0
}
