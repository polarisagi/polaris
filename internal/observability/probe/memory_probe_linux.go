//go:build linux

package probe

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
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

// cgroup 伪文件所在目录：v2 统一层级根、v1 memory 子系统。
const (
	cgroupV2Dir = "/sys/fs/cgroup"
	cgroupV1Dir = "/sys/fs/cgroup/memory"
)

// probeCgroupMemory 读取当前进程所属 cgroup 的内存上限与可用量（v2 优先，回退 v1）。
// ok=false 表示不在受限 cgroup 内（未容器化，或上限为 "max"/极大值）。
func probeCgroupMemory() (limit, available uint64, ok bool) {
	return probeCgroupMemoryAt(cgroupV2Dir, cgroupV1Dir)
}

// probeCgroupMemoryAt 可用量 = 上限 − 工作集，工作集 = 已用 − inactive_file。
//
// memory.current / usage_in_bytes 含 page cache：SQLite 等文件读多了它会贴近上限，
// 直接相减会把可回收的缓存算成已耗尽，容器内可用内存趋近 0，ResourceGovernor
// 长期判 L3、拒绝全部后台工作。扣除 inactive_file 与 kubelet 的 working set 口径一致
// （OOM 前内核会先回收这部分）。memory.stat 读不到时按保守口径不扣除。
func probeCgroupMemoryAt(v2Dir, v1Dir string) (limit, available uint64, ok bool) {
	// cgroup v2：统一层级，上限 "max" 表示不限（解析失败即视为不限）。
	if l, ok := readUintFile(filepath.Join(v2Dir, "memory.max")); ok && l > 0 {
		used, _ := readUintFile(filepath.Join(v2Dir, "memory.current"))
		inactive := readMemoryStatField(filepath.Join(v2Dir, "memory.stat"), "inactive_file")
		return l, saturatingSub(l, saturatingSub(used, inactive)), true
	}
	// cgroup v1：未设上限时是一个接近 uint64 max 的哨兵值（PAGE_COUNTER_MAX×PAGE_SIZE）。
	if l, ok := readUintFile(filepath.Join(v1Dir, "memory.limit_in_bytes")); ok && l > 0 && l < 1<<62 {
		used, _ := readUintFile(filepath.Join(v1Dir, "memory.usage_in_bytes"))
		inactive := readMemoryStatField(filepath.Join(v1Dir, "memory.stat"), "total_inactive_file")
		return l, saturatingSub(l, saturatingSub(used, inactive)), true
	}
	return 0, 0, false
}

// readMemoryStatField 读取 memory.stat 中 "<field> <bytes>" 行的值；缺失返回 0。
func readMemoryStatField(path, field string) uint64 {
	b, err := os.ReadFile(path) //nolint:gosec // 固定的 cgroup 伪文件路径，非外部输入
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		name, val, found := strings.Cut(line, " ")
		if !found || name != field {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
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
