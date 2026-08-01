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
	// 污点等级 → 施加限制（配额折半）：语义为"只增不减"，用 >=，避免比 High 更高的
	// 等级（若未来新增）反而不受限。TaintUserReviewed 数值上大于 TaintHigh 但代表
	// "人工已复核"（豁免语义而非风险语义），必须先排除，否则复核后配额反而更紧，
	// 与 execute/dag/taint_downgrade.go 的 "先判 UserReviewed 豁免、再判 >= High 限制"
	// 既定 idiom 相悖。
	if taintLevel != types.TaintUserReviewed && taintLevel >= types.TaintHigh {
		q.MemoryPages /= 2
		q.Fuel /= 2
		q.MaxMounts /= 2
	}
	return q
}
