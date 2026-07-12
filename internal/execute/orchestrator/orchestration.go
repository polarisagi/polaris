package orchestrator

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
	ModePatternDAG                          // 跨 Agent 强类型 DAG
	ModeStateGraph                          // 条件路由 + 有界循环状态图（GD-8-001）
)
