package swarm

// 编排模式实现。
// 架构文档: docs/arch/08-Multi-Agent-Orchestrator-深度选型.md §3, §1.3-1.9

// OrchestrationMode 编排模式。
type OrchestrationMode int

const (
	ModeSupervisor OrchestrationMode = iota // 默认: Planner→Worker→汇总
	ModeHierarchy                           // 递归分解
	ModeSequential                          // A输出→B输入
	ModeParallel                            // 独立子任务并发
	ModeMapReduce                           // 分片归并
	ModeReflection                          // 执行→审查→改进
	ModeSwarm                               // 去中心化handoff
)

// TopologyEvolver 编排拓扑自演化。
// Evaluate: 获取候选 fitness → Pareto 前沿 (成功率 × token 效率)
// → 候选 ≥ 当前 5pp → TopologyChange (A/B 50/50 分流 50 任务).
type TopologyEvolver struct {
	fitnessMap map[string]*TopologyFitness
}

// Evaluate 评估候选拓扑：Pareto 前沿（成功率 × token 效率）双维度比较。
// SampleSize < 10 的候选不参与评估，防冷启动噪音。
// 候选在成功率上领先 baseline ≥5pp 且 token 效率不劣化（cost ≤ base×1.1）时返回 true。
func (te *TopologyEvolver) Evaluate(candidate *TopologyFitness, baseline string) bool {
	if te.fitnessMap == nil {
		te.fitnessMap = make(map[string]*TopologyFitness)
	}
	te.fitnessMap[candidate.Topology] = candidate
	if candidate.SampleSize < 10 {
		return false // 样本不足，不参与评估
	}
	base, ok := te.fitnessMap[baseline]
	if !ok {
		return true // 无基线，接受新候选
	}
	if base.SampleSize < 10 {
		return true // 基线样本不足，候选直接接受
	}
	// Pareto 双维：成功率领先 ≥5pp 且 token 成本不劣化超 10%
	successLead := candidate.SuccessRate >= base.SuccessRate+0.05
	tokenOK := base.AvgTokenCost == 0 || candidate.AvgTokenCost <= base.AvgTokenCost*1.1
	return successLead && tokenOK
}

// TopologyFitness 拓扑适应度。
type TopologyFitness struct {
	Topology         string
	TaskType         string
	SuccessRate      float64
	AvgLatencyMs     int64
	AvgTokenCost     float64
	AgentUtilization float64 // 0-1，单任务内 Agent 活跃占比
	SampleSize       int     // <10 不参与 Pareto 评估
}
