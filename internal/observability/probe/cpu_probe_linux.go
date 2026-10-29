//go:build linux

package probe

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// cpuSampleState 保存上一次 /proc/stat 快照（累计时钟滴答）。
type cpuSampleState struct {
	idle  uint64
	total uint64
}

// sampleCPU 解析 /proc/stat 首行，按与上次快照的增量计算占用率。
// 格式：cpu user nice system idle iowait irq softirq steal ...
func sampleCPU(prev cpuSampleState) (pct float64, next cpuSampleState, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, prev, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, prev, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, prev, false
	}

	var cur cpuSampleState
	for i, fv := range fields[1:] {
		v, convErr := strconv.ParseUint(fv, 10, 64)
		if convErr != nil {
			continue
		}
		cur.total += v
		if i == 3 { // fields[4] = idle
			cur.idle = v
		}
	}

	// 首次采样无增量可算，只记录基线。
	if prev.total == 0 || cur.total <= prev.total {
		return 0, cur, false
	}
	dTotal := cur.total - prev.total
	dIdle := cur.idle - prev.idle
	return float64(dTotal-dIdle) / float64(dTotal) * 100, cur, true
}
