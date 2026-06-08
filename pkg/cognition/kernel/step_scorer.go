package kernel

// stepScorer 执行步骤实时打分，用于 Adaptive Max-Steps 动态预算收紧。
// 权重: toolSuccess=0.4, schemaCheck=0.3, latency=0.2, tokenEfficiency=0.1。
// 架构文档: docs/arch/M04-Agent-Kernel.md §5.5
type stepScorer struct {
	toolSuccessWeight float64
	schemaCheckWeight float64
	latencyWeight     float64
	tokenEfficiencyWt float64
}

func newDefaultStepScorer() *stepScorer {
	return &stepScorer{
		toolSuccessWeight: 0.4,
		schemaCheckWeight: 0.3,
		latencyWeight:     0.2,
		tokenEfficiencyWt: 0.1,
	}
}

// stepCtx 单步执行上下文。
type stepCtx struct {
	ToolName     string
	LatencyMs    int64
	TokensUsed   int
	SchemaPassed bool
	ToolResult   bool
}

// score 计算步骤分数（1.0 起点，各项扣分）。
// 返回值范围 [0, 1]；低于 stepBudgetShrinkThreshold 时触发预算收紧。
func (s *stepScorer) score(c stepCtx) float64 {
	val := 1.0

	if !c.ToolResult {
		val -= s.toolSuccessWeight
	}
	if !c.SchemaPassed {
		val -= s.schemaCheckWeight
	}

	latencyPenalty := float64(c.LatencyMs) / 5000.0
	if latencyPenalty > 1.0 {
		latencyPenalty = 1.0
	}
	val -= s.latencyWeight * latencyPenalty

	tokenRatio := float64(c.TokensUsed) / 1024.0
	if tokenRatio > 1.0 {
		tokenRatio = 1.0
	}
	val -= s.tokenEfficiencyWt * tokenRatio

	return val
}

// stepBudgetShrinkThreshold: 步骤分数低于此值时触发预算收紧（减少 10% MaxStepsLimit）。
const stepBudgetShrinkThreshold = 0.5

// adjustMaxSteps 根据步骤分数动态调整 MaxStepsLimit。
// 低分→收紧（防止低质量无限循环）；高分→不扩展（防止预算膨胀）。
func adjustMaxSteps(current int, score float64) int {
	if current <= 0 {
		return current // 无上限模式，不调整
	}
	if score < stepBudgetShrinkThreshold && current > 1 {
		shrunk := current - (current / 10)
		if shrunk < 1 {
			shrunk = 1
		}
		return shrunk
	}
	return current
}
