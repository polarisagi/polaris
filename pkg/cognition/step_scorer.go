package cognition

// StepScorer — 执行步骤实时打分。
// 用于 Best-of-N 剪枝和 MEMF 候选标记。
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §5.5

// StepScorer 评分器。
// 权重: toolSuccess=0.4, schemaCheck=0.3, latency=0.2, tokenEfficiency=0.1.
type StepScorer struct {
	toolSuccessWeight float64 // 0.4
	schemaCheckWeight float64 // 0.3
	latencyWeight     float64 // 0.2
	tokenEfficiencyWt float64 // 0.1
}

// StepContext 步骤上下文。
type StepContext struct {
	ToolName     string
	Input        []byte
	Output       []byte
	LatencyMs    int64
	TokensUsed   int
	SchemaPassed bool
	ToolResult   bool // true=success, false=failure
}

// Score 计算步骤分数 (1.0 起点，各项扣分)。
// 双路径: Best-of-N 剪枝 + 低分标记 MEMF 候选。
func (s *StepScorer) Score(ctx StepContext) float64 {
	score := 1.0

	// tool success: +0 if success, -0.4 if failure
	if !ctx.ToolResult {
		score -= s.toolSuccessWeight
	}

	// schema check: +0 if passed, -0.3 if failed
	if !ctx.SchemaPassed {
		score -= s.schemaCheckWeight
	}

	// latency: -0.2 × (latency / maxExpectedLatency), capped at 0.2
	latencyPenalty := float64(ctx.LatencyMs) / 5000.0 // maxExpectedLatency = 5s
	if latencyPenalty > 1.0 {
		latencyPenalty = 1.0
	}
	score -= s.latencyWeight * latencyPenalty

	// token efficiency: -0.1 × (tokens / expectedTokens), capped at 0.1
	tokenRatio := float64(ctx.TokensUsed) / 1024.0 // expectedTokens = 1024
	if tokenRatio > 1.0 {
		tokenRatio = 1.0
	}
	score -= s.tokenEfficiencyWt * tokenRatio

	return score
}
