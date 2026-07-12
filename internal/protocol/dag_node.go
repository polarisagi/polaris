package protocol

import (
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// DAG 可执行节点跨模块契约（M04 §5.3, M08 §8.2）。
//
// producer: internal/execute/dag（DAGExecutor 具体调度实现，类型别名于此；
//           2026-07-12 随 internal/execute 模块化从 internal/agent/dag 迁出）
// consumer: internal/swarm/planner（TaskDecomposer 分解目标为节点列表）、
//           internal/agent/fsm（DAGModel 填槽产出）
//
// ExecNode 此前以 internal/agent/dag.ExecNode 具体类型由 internal/swarm/planner
// 直接 import 消费，违反 M04 §B2。现收敛至此，execute/dag 与 swarm/planner 均引用
// 本文件定义，execute/dag 不再是唯一权威源但保留同名别名保证向后兼容。
//
// DAGPlan/ExecEdge/EdgePolarity 此前存在遗留缺陷：本文件已定义这三个类型，但
// execute/dag（原 agent/dag）内部保留了独立的同名重复定义（未按 ExecNode 的
// 方式收敛为类型别名），导致本文件这三个类型定义长期处于零引用的死代码状态
// （2026-07-12 排查确认）。本次随 internal/execute 迁移一并修复：execute/dag
// 改为对本文件类型做别名（见 execute/dag/executor.go），此处恢复为唯一权威源。
// NodeResult/DAGValidationContext/DAGValidationError 见 dag_validation.go（同一
// 迁移中新增，原为 agent/dag 包内非跨模块类型，因 execute/dag 物理迁出成为独立
// 模块后按 HE-3 收敛至此）。

// NodeStatus 定义节点执行状态。
type NodeStatus int

const (
	NodePending NodeStatus = iota
	NodeRunning
	NodeCompleted
	NodeFailed
	NodeSkipped // 因上游失败而跳过
)

// CompensationAction 描述一个节点失败后的 Saga 逆序补偿动作。
// write_local / write_network 节点必须声明此字段，否则 DAG 校验拒绝。
type CompensationAction struct {
	ToolName   string
	Args       []byte
	TaintLevel types.TaintLevel
}

// ExecNode 是 DAG 中可执行的工具调用节点。
type ExecNode struct {
	ID             string
	ToolName       string
	Args           []byte
	TaintLevel     types.TaintLevel     // 从 Context 继承的污染等级
	DependsOn      []string             // 前驱节点 ID
	Compensation   *CompensationAction  // Saga 补偿动作（有副作用节点必填）
	MaxRetry       int                  // 默认 0（不重试）
	Timeout        time.Duration        // 0 使用全局默认
	Status         NodeStatus           // 节点状态
	IdempotencyKey types.IdempotencyKey // 幂等键
}

// EdgePolarity 描述 DAG 边的语义
type EdgePolarity int

const (
	EdgeData     EdgePolarity = iota // 数据依赖：上游产出作为下游输入
	EdgeSequence                     // 纯时序约束（无数据传递）
)

// ExecEdge 是 DAG 中的有向边。
type ExecEdge struct {
	From     string
	To       string
	Polarity EdgePolarity
}

// DAGPlan 是完整的可执行 DAG 计划。
type DAGPlan struct {
	Nodes []ExecNode
	Edges []ExecEdge
}

// WorkflowNodeSpec 表示强类型跨 Agent 编排模式的节点。
type WorkflowNodeSpec struct {
	ID             string              `json:"id"`
	CapabilityType string              `json:"capability_type"` // Agent 能力类型（对应 tasks.type）
	IntentTemplate string              `json:"intent_template"` // 任务模板，支持上下文注入
	MaxRetry       int                 `json:"max_retry"`
	Compensation   *CompensationAction `json:"compensation,omitempty"`
	// MaxVisits 节点在一次 StateGraph 执行中允许被（重复）触发的最大次数（GD-8-001，
	// 编排模式10 StateGraphExecutor 专用；PatternDAGExecutor/编排模式9 忽略此字段）。
	// 0 或 1 = 至多执行一次，语义与既有 DAG 完全等价（向后兼容）；>1 允许被循环边
	// 重复触达，由 StateGraphExecutor 以硬计数器强制上限（HE-Rule-2：物理熔断，
	// 而非依赖拓扑分析"猜测"是否可能死循环）。与 Compensation 同时声明（>1 且非
	// nil Compensation）视为非法配置，StateGraphExecutor 校验阶段拒绝——多次执行的
	// 节点的 Saga 逆序补偿语义未定义。
	MaxVisits int `json:"max_visits,omitempty"`
	// IsEntry 显式声明本节点为状态图执行入口（GD-8-001 StateGraph 专用）。
	// 纯粹以入度=0 判定入口在存在循环边时会失效——参与循环反馈的节点（如
	// "executor"）通常同时被外部入口和循环边指向，入度恒 > 0，但仍应在执行开始
	// 时被首批触发。显式标记避免"入度分析"这类隐式推断在环图场景下静默失效。
	// 入度=0 的节点无需显式标记（仍按入口处理，向后兼容既有 DAG 语义）。
	IsEntry bool `json:"is_entry,omitempty"`
}

// EdgeConditionOp 边条件比较操作符（GD-8-001 StateGraph 条件边）。
type EdgeConditionOp string

const (
	// CondEquals 上游节点输出 JSON 中 Field 字段值（字符串化后）等于 Value。
	CondEquals EdgeConditionOp = "eq"
	// CondNotEquals 同上，取反。
	CondNotEquals EdgeConditionOp = "ne"
	// CondGreaterThan/CondLessThan/CondGreaterOrEqual/CondLessOrEqual（GD-14-002 复核扩展）：
	// Field/Value 均按 float64 解析后比较；任一侧无法解析为数字时 fail-closed（返回 false），
	// 与既有"字段缺失/JSON 解析失败 fail-closed"原则一致（宁可漏触发，不误触发）。
	CondGreaterThan    EdgeConditionOp = "gt"
	CondLessThan       EdgeConditionOp = "lt"
	CondGreaterOrEqual EdgeConditionOp = "ge"
	CondLessOrEqual    EdgeConditionOp = "le"
	// CondContains Field 字段值（字符串化后）包含 Value 子串。
	CondContains EdgeConditionOp = "contains"
	// CondExists Field 字段在上游输出 JSON 中存在（不比较值，Value 被忽略）。
	CondExists EdgeConditionOp = "exists"
)

// EdgeCondition 声明式边条件（GD-8-001 初版 + GD-14-002 复核扩展，编排模式10
// StateGraphExecutor 专用）。
//
// HE-Rule-2（可验证执行）边界：GD-14-002 原始 finding 建议引入 CEL 表达式引擎，
// 经复核确认 ADR-0041 已就此做出明确否决（禁止脚本/表达式引擎作为条件求值器，
// 避免"可验证执行"退化为"运行任意代码决定控制流"）——该决策维持不变，本次扩展
// 不引入表达式解析器，仅在原有 Field/Op/Value 声明式模型上：
//  1. 新增数值比较算子（gt/lt/ge/le）与子串/存在性算子（contains/exists）；
//  2. 新增 And/Or 结构化复合条件（子条件静态嵌套，非运行时解析的自由语法）。
//
// 二者与既有 eq/ne 同属"预定义、可枚举、可静态分析"的声明式比较，不具备变量绑定、
// 函数调用、控制流等表达式引擎的核心特征，因此不改变 HE-Rule-2 合规边界。
//
// And/Or 与 Field/Op/Value 互斥：非 nil 时优先生效（And 优先于 Or），Field/Op/Value
// 被忽略。三者皆为空视为无条件（恒真），保持 nil Condition 语义一致。
// nil Condition = 无条件边，等价于既有 WorkflowEdgeSpec 静态依赖语义（向后兼容）。
type EdgeCondition struct {
	Field string          `json:"field"` // 上游节点输出 JSON 顶层字段名
	Op    EdgeConditionOp `json:"op"`    // 比较操作符
	Value string          `json:"value"` // 期望值（上游字段值经 fmt.Sprintf("%v", ...) 归一化后比较）

	// And/Or 结构化复合条件（GD-14-002 复核扩展）：全部/任一子条件为真时本条件为真。
	// 与顶层 Field/Op/Value 互斥（见上方类型注释），支持递归嵌套表达任意深度的
	// 布尔组合，但节点数量受 JSON 反序列化的自然大小限制约束，不存在无界递归风险。
	And []*EdgeCondition `json:"and,omitempty"`
	Or  []*EdgeCondition `json:"or,omitempty"`
}

// WorkflowEdgeSpec 表示工作流节点间的依赖。
type WorkflowEdgeSpec struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Condition 非 nil 时，仅当上游（From）节点输出满足该条件才触发本边（GD-8-001
	// StateGraph 专用；PatternDAGExecutor/编排模式9 忽略此字段，视为无条件边）。
	Condition *EdgeCondition `json:"condition,omitempty"`
}

// WorkflowGraphSpec 定义跨 Agent 的强类型 DAG 编排图。
// 编排模式9（PatternDAGExecutor）要求严格无环；编排模式10（StateGraphExecutor）
// 允许通过 Condition + MaxVisits 表达的有界循环，两者共用本结构体（M08 §3-quinquies）。
type WorkflowGraphSpec struct {
	Nodes []WorkflowNodeSpec `json:"nodes"`
	Edges []WorkflowEdgeSpec `json:"edges"`
}
