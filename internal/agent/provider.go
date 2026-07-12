package agent

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 agent 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// 设计目的：
//   当前 agent/agent.go 直接 import extension/skill、action/codeact、action/lam
//   导致 Agent 核心依赖了多个具体实现包，难以测试、难以追踪调用链。
//
//   解决方案：
//   1. 此文件声明 Agent 需要的所有外部能力接口（consumer-side）
//   2. 本地桥接类型（CodeActRequest/CodeActResult/SkillHandle）避免 agent 包
//      import action/codeact、extension/skill 等具体实现包
//   3. agent.go 的 struct 字段改为这些接口类型
//   4. 具体实现由 cli.go/bootstrap 构造并通过 adapter 注入
//   5. agent 包不再 import extension/skill、action/codeact、action/lam
//
// @consumer: agent/agent.go（字段类型）
// @producer: cmd/polaris/adapters.go（adapter 层注入）

// ─── 桥接类型（Bridge Types）────────────────────────────────────────────────
// 这些类型是 agent 包的本地定义，与 codeact/skill 具体包的类型字段一一对应。
// adapter 层（cmd/polaris/adapters.go）负责在两者之间做字段映射。

// CodeActRequest agent 包本地定义的代码执行请求。
// 字段与 action/protocol.CodeActRequest 完全对应，避免 agent 包 import codeact。
type CodeActRequest struct {
	Language     string // "python" | "bash"
	Code         string // LLM 生成的代码文本
	CapabilityID string // 能力令牌 ID（必须有效）
	SessionID    string
	AgentID      string
	TaintLevel   types.TaintLevel
	// StatefulSession 见 action/protocol.CodeActRequest 同名字段注释（GD-4-002）。
	StatefulSession bool
}

// CodeActResult agent 包本地定义的代码执行结果。
// 字段与 action/protocol.CodeActResult 完全对应。
type CodeActResult struct {
	Output    []byte
	ExitCode  int
	LatencyMs int64
}

// SkillHandle agent 包本地定义的技能进程句柄（轻量 token）。
// 字段与 extension/skill.ProcessHandle 完全对应，避免 agent 包 import extension/skill。
// agent 只关心 SkillID（传给 SkillExecutor.ExecuteSkill），其余字段由 adapter 层处理。
type SkillHandle struct {
	SkillID string
}

// ─── 消费端接口（Consumer-side Interfaces）──────────────────────────────────

// CodeActEngine Agent 对 LLM 代码执行引擎的消费端接口。
// 实现：action/codeact.CodeAct（通过 cmd/polaris.codeActAdapter 包装）
// 禁止：agent 直接 import action/codeact
type CodeActEngine interface {
	// Execute 在沙箱中执行 LLM 生成的代码，返回执行结果。
	Execute(ctx context.Context, req CodeActRequest) (*CodeActResult, error)
	// IsAvailable 返回引擎是否就绪（依赖沙箱、编译器等）。
	IsAvailable() bool
}

// ScriptSkillCache Agent 对 Python 技能脚本缓存的消费端接口。
// 实现：extension/skill.ScriptSkillCache（通过 cmd/polaris.skillCacheAdapter 包装）
// 禁止：agent 直接 import extension/skill
type ScriptSkillCache interface {
	// GetOrSpawn 检查技能是否可用并返回进程句柄（token）。
	// 返回 nil handle 表示技能不在缓存中，应退回合成 JSON 路径。
	GetOrSpawn(ctx context.Context, skillID string) (*SkillHandle, error)
}

// LAMPolicyChecker Agent 对 LAM GUI 自动化策略预检的消费端接口。
// 实现：action/lam.ComputerUseEngine（通过 cmd/polaris.lamPolicyAdapter 包装）
// 禁止：agent 直接 import action/lam
// 注：LAM 完整执行（ExecuteAction）走 tool/builtin 路径，不在此接口。
type LAMPolicyChecker interface {
	// CheckPolicy 对 GUI 动作做 Cedar 策略预检（deny-by-default，先于 HITL 审批）。
	CheckPolicy(ctx context.Context, actionJSON []byte) error
}

// ToolCatalog Agent 对工具目录的消费端接口。
// 实现：tool/catalog.Catalog（tool/catalog 包仅含接口，无具体实现循环风险）
// agent.go 字段类型直接使用 catalog.Catalog 接口，此处保留文档说明。
//
// 注意：agent 包允许 import tool/catalog（因为 tool/catalog 只定义接口，
// 不 import agent，无循环风险）。此 ToolCatalog 接口为备用文档，不用于字段类型。

// WorldModelUpdater Agent 对认知世界模型的消费端接口。
// 实现：memory/graph.WorldModel（基于 SurrealDB 图的世界模型）
// 禁止：agent 直接 import memory/graph
type WorldModelUpdater interface {
	// Update 根据工具执行结果更新世界模型节点。
	Update(ctx context.Context, toolName string, result *types.ToolResult) error
	// CheckBlindZone 检查任务描述是否落入已知盲区（返回命中的盲区描述）。
	CheckBlindZone(ctx context.Context, taskDesc string) ([]string, error)
}

// ─── DAG 执行引擎消费端接口（2026-07-12 随 internal/execute 模块化新增）───────
//
// internal/agent/dag 物理迁出为 internal/execute/dag 顶层模块前，FSM 思考循环
// （fsm/state_machine.go、agent_execute_dag.go 等）直接 import 同目录子包，
// 不算跨模块依赖。迁出后按本文件既有模式收敛为消费端接口：agent 不再直接
// import internal/execute/dag，具体实现（execute/dag.Runner/Validator）由
// cmd/polaris/boot_agent.go 构造并通过 InjectDAGRunner/InjectDAGValidator 注入。
//
// DAGToolExecutorFn/DAGLeaseRenewFn 是本地具名类型，仅供调用方（agent_execute_dag.go）
// 声明局部变量时使用，便于阅读。DAGRunner 接口方法本身必须使用与之底层结构相同的
// 匿名函数类型声明参数——Go 接口方法签名比较要求类型"完全相同"，具名类型与匿名类型
// 即使底层结构一致也不相同；若接口方法直接用 DAGToolExecutorFn 具名类型，
// execute/dag.Runner.Run（其参数为匿名函数类型，见 execute/dag/runner.go）将无法
// 结构化满足本接口，届时只能靠 execute/dag 反向 import agent 包类型来对齐签名，
// 这会把执行引擎耦合回它服务的认知核心，方向颠倒。故接口方法签名保持匿名函数类型。

// DAGToolExecutorFn 单次工具调用签名，与 execute/dag.ToolExecutorFn 结构一致。
type DAGToolExecutorFn func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error)

// DAGLeaseRenewFn 任务租约续期签名，与 execute/dag.LeaseRenewFn 结构一致。
type DAGLeaseRenewFn func(ctx context.Context, taskID, agentID string, ttl time.Duration) error

// DAGRunner Agent 对单 Agent 内工具链 DAG 执行引擎的消费端接口。
// 实现：execute/dag.Runner（无状态，cmd/polaris/boot_agent.go 构造后注入）
// 禁止：agent 直接 import internal/execute/dag
type DAGRunner interface {
	// Run 执行一次完整 DAG 计划，返回节点结果、是否触发降级重规划、错误。
	// toolExec/leaseRenew 刻意使用匿名函数类型（见上），调用方可直接传入
	// DAGToolExecutorFn/DAGLeaseRenewFn 类型的值（赋值兼容，二者底层结构相同）。
	Run(
		ctx context.Context,
		plan *protocol.DAGPlan,
		toolExec func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error),
		leaseRenew func(ctx context.Context, taskID, agentID string, ttl time.Duration) error,
		taskID, agentID string,
	) (results []protocol.NodeResult, degradedReplan bool, err error)
}

// DAGValidator Agent 对 S_VALIDATE 四层校验管线的消费端接口。
// 实现：execute/dag.Validator（无状态，cmd/polaris/boot_agent.go 构造后注入）
// 禁止：agent 直接 import internal/execute/dag
type DAGValidator interface {
	// Validate 执行 L0 拓扑/L1 Taint/L1 Policy/L2 Heuristic/L3 LLM 看门狗校验。
	Validate(ctx context.Context, vCtx *protocol.DAGValidationContext) error
}
