package sandbox

import (
	"github.com/polarisagi/polaris/pkg/types"
)

// WasmQuota 定义 Wasm 执行资源的动态配额。
type WasmQuota struct {
	MemoryPages int
	Fuel        int
	MaxMounts   int
}

// CalculateWasmQuota 结合硬件 Tier 和 TaintLevel 计算可用配额。
// tier 0: Mem 128MB (2048 pages), Fuel 10M
// tier 1+: Mem 512MB (8192 pages), Fuel 50M
// 如果 taintLevel == High，Quota 折半
func CalculateWasmQuota(tier int, taintLevel types.TaintLevel) WasmQuota {
	var q WasmQuota
	if tier == 0 {
		q.MemoryPages = 2048
		q.Fuel = 10000000
		q.MaxMounts = 1
	} else {
		q.MemoryPages = 8192
		q.Fuel = 50000000
		q.MaxMounts = 5
	}
	if taintLevel == types.TaintHigh {
		q.MemoryPages /= 2
		q.Fuel /= 2
		q.MaxMounts /= 2
	}
	return q
}
