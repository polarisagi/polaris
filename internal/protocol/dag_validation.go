package protocol

import "github.com/polarisagi/polaris/pkg/types"

// DAG 执行结果 / S_VALIDATE 校验上下文跨模块契约。
//
// producer: internal/execute/dag（DAGExecutor.Execute 产出 NodeResult；
//           ValidateDAG 消费 DAGValidationContext；2026-07-12 随 internal/execute
//           模块化从 internal/agent/dag 迁出）
// consumer: internal/agent（runExecuteDAG/runValidateDAG 通过 agent/provider.go
//           声明的 DAGRunner/DAGValidator 消费端接口间接使用，不再直接 import
//           internal/execute/dag）
//
// 迁出动机：NodeResult/DAGValidationContext/DAGValidationError 此前是
// agent/dag 包内类型，agent 包直接 import 同目录子包不算跨模块依赖。
// dag 物理迁出为独立顶层模块 internal/execute 后，agent 若继续直接 import
// 具体实现即违反 agent/CLAUDE.md 的消费端接口边界（HE-3），故随迁移一并
// 收敛至此，execute/dag 保留同名类型别名。

// NodeResult 记录单个 DAG 节点的执行结果。
type NodeResult struct {
	NodeID     string
	Output     []byte
	LatencyMs  int64
	Suspended  bool
	Err        error
	TaintLevel types.TaintLevel
	ImageParts []types.ImagePart
}

// DAGValidationContext 承载 S_VALIDATE 四层校验所需的输入。
// 架构文档: docs/arch/M04-Agent-Kernel.md §4
type DAGValidationContext struct {
	// Plan 是 S_PLAN 阶段 LLM 产出的 DAG。
	Plan *DAGPlan
	// ActiveTaintLevel 是当前会话上下文中传播而来的最高污点等级（Layer A 规则）。
	// 计算规则: max(所有输入 TaintLevel) —— 只升不降。
	ActiveTaintLevel types.TaintLevel
	// PolicyGate 是 Cedar 策略引擎的 Go 接口（L1 确定性 Cedar 校验）。
	PolicyGate PolicyGate
	// ToolExecutor 用于 L1_taint 校验中动态判断工具的只读属性（替代硬编码白名单）。
	// 为 nil 时退化为内置白名单兜底。
	ToolExecutor AgentToolExecutor
	// AgentID 用于 PolicyGate.Review 中的 principal 字段。
	AgentID string
	// SessionID 用于审计事件的关联查询。
	SessionID string
	// SystemTier 系统环境配置级别 (0: 8GB 弱计算节点, 1+: 强计算节点)
	SystemTier int
	// Provider 用于 L3 看门狗调用。
	Provider Provider
	// MonthlySpendUSD 当前估算 USD 消耗（由 BudgetManager.EstimatedSpendUSD() 填充）。
	MonthlySpendUSD float64
	// MonthlyBudgetUSD 来自配置项，0 = 不限额。
	MonthlyBudgetUSD float64
}

// DAGValidationError 包装 S_VALIDATE 失败的结构化错误。
type DAGValidationError struct {
	Layer  string // "L0" | "L1_taint" | "L1_policy" | "L2_heuristic" | "L3_llm"
	NodeID string // 首个违规节点 ID（空表示全局失败）
	Reason string
}

func (e *DAGValidationError) Error() string {
	if e.NodeID != "" {
		return "validate [" + e.Layer + "] node=" + e.NodeID + ": " + e.Reason
	}
	return "validate [" + e.Layer + "]: " + e.Reason
}
