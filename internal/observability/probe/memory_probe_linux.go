//go:build linux

package probe

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func probeOSMemory() (total uint64, available uint64) {
	total, available = probeHostMemory()
	if total == 0 {
		return fallbackMemoryProbe()
	}
	// 容器内 /proc/meminfo 报的是**宿主机**内存（procfs 未被 lxcfs 之类改写时），
	// 而进程实际能用的是 cgroup 上限。Tier-0 的目标部署形态就包含 2GB VPS 上的
	// 容器，若按宿主机 64GB 判定 Tier，会直接把本地推理/大并发等能力错误解锁，
	// 随后被 OOM Killer 收场。取两者较小值。
	if cgTotal, cgAvail, ok := probeCgroupMemory(); ok && cgTotal < total {
		return cgTotal, cgAvail
	}
	return total, available
}

func probeHostMemory() (total uint64, available uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		var si unix.Sysinfo_t
		if sysErr := unix.Sysinfo(&si); sysErr == nil {
			total = si.Totalram * uint64(si.Unit)
			available = (si.Freeram + si.Bufferram) * uint64(si.Unit)
		}
		return total, available
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemAvailable:"):
			available = parseMeminfoKB(line)
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMeminfoKB(line)
		}
	}
	return total, available
}

func parseMeminfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	val, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return val * 1024 // kB → bytes
}

// probeCgroupMemory 读取当前进程所属 cgroup 的内存上限与已用量（v2 优先，回退 v1）。
// ok=false 表示不在受限 cgroup 内（未容器化，或上限为 "max"/极大值）。
func probeCgroupMemory() (limit, available uint64, ok bool) {
	// cgroup v2：统一层级，上限 "max" 表示不限（解析失败即视为不限）。
	if l, ok := readUintFile("/sys/fs/cgroup/memory.max"); ok && l > 0 {
		used, _ := readUintFile("/sys/fs/cgroup/memory.current")
		return l, saturatingSub(l, used), true
	}
	// cgroup v1：未设上限时是一个接近 uint64 max 的哨兵值（PAGE_COUNTER_MAX×PAGE_SIZE）。
	if l, ok := readUintFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok && l > 0 && l < 1<<62 {
		used, _ := readUintFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		return l, saturatingSub(l, used), true
	}
	return 0, 0, false
}

// readUintFile 读取 cgroup 伪文件里的单个无符号整数。
// 返回 ok=false 表示"文件不存在 / 内容不是数字"（典型：cgroup v2 的 "max"），
// 两者对调用方是同一种情况——没有可用的上限值，故不返回 error。
func readUintFile(path string) (uint64, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // 固定的 cgroup 伪文件路径，非外部输入
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

func saturatingSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}
