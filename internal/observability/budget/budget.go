package budget

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
)

// ResourceBudget 后台任务认知负载门控。boot 期注入真实单例/探针（C1.3）。
// 任一依赖为 nil（Tier0 降级）→ 保守按"压力未知，允许"返回 true（不因可观测缺失误杀核心）。
type ResourceBudget struct {
	AvailableMB   int
	IsConstrained bool

	burn  *metrics.TokenBurnRate
	guard *probe.OSMemoryGuard
	gate  *probe.FeatureGate
}

// NewResourceBudget 构造 ResourceBudget，注入真实 TokenBurnRate、OSMemoryGuard、FeatureGate。
func NewResourceBudget(burn *metrics.TokenBurnRate, guard *probe.OSMemoryGuard, gate *probe.FeatureGate) *ResourceBudget {
	return &ResourceBudget{burn: burn, guard: guard, gate: gate}
}

// BackgroundPermit 是否允许 Priority-p 后台任务本轮运行。
//
//   - p=2：后台优化（M9 Evolver、M5 Consolidation）
//   - p=3：最低优先级（M10 GraphRAG 重建、Auto-Curriculum）
//   - p≤1：用户触发路径，恒允许，不调用本函数
//
// 判断维度：认知压力(float64) + OS 内存降级等级 + token 燃烧率 vs P95 基线。
func (b *ResourceBudget) BackgroundPermit(priority int) bool {
	cog := metrics.GlobalCognitivePressure().Current()

	mem := 0
	if b.guard != nil && b.gate != nil {
		// DegradationNone=0, Caution=1, Warning=2, Critical=3
		mem = int(b.guard.CurrentPressureLevel(b.gate.GetAvailableMemoryMB()))
	}

	var burn, p95 float64
	if b.burn != nil {
		burn = b.burn.EMA5s()
		p95 = b.burn.BaselineP95()
	}

	switch priority {
	case 2:
		// 后台优化：允许中等压力，燃烧率不超 P95 的 2 倍
		return cog < 2.0 && mem < 2 && (p95 == 0 || burn < p95*2.0)
	case 3:
		// 最低优先级：仅在系统极空闲时运行，燃烧率不超 P95 的 1.5 倍
		return cog < 0.5 && mem < 1 && (p95 == 0 || burn < p95*1.5)
	default:
		return true // priority <= 1 恒允许（用户交互/前台辅助路径）
	}
}
