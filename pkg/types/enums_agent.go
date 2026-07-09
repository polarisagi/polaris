package types

// ============================================================================
// M4 Agent Kernel — FSM 状态枚举
// 来源: internal/protocol/types.go §M4
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §1
// 11 状态: 5 主执行态 + 2 恢复态 + 2 终态 + 1 中断暂停态 + 1 挂起态
//
// 从 enums.go 按模块拆出（R7 文件行数治理，2026-07-07），纯类型/常量声明，
// 无逻辑变更；docs/arch/00-Global-Dictionary.md 仍是概念权威源，本文件只是
// 该权威定义在 pkg/types 中的物理落点。
// ============================================================================

// AgentState 定义 Agent FSM 的全部状态枚举（全系统唯一权威定义）。
type AgentState int

const (
	AgentStateIdle      AgentState = iota // S_IDLE: 空闲等待意图
	AgentStatePerceive                    // S_PERCEIVE: LLM 填槽理解任务
	AgentStatePlan                        // S_PLAN: LLM 填槽生成 DAG
	AgentStateValidate                    // S_VALIDATE: 四层校验
	AgentStateExecute                     // S_EXECUTE: DAG 执行
	AgentStateReflect                     // S_REFLECT: LLM 填槽反思
	AgentStateReplan                      // S_REPLAN: 重新规划（Recovery）
	AgentStateRollback                    // S_ROLLBACK: Saga 逆序补偿（Recovery）
	AgentStateComplete                    // S_COMPLETE: 成功终态
	AgentStateFailed                      // S_FAILED: 失败终态（ReplanGuard 超限）
	AgentStateInterrupt                   // S_INTERRUPT: 用户中断暂停态（非终态，可 Resume/Redirect/Abort）
	AgentStateSuspended                   // S_SUSPENDED: 空闲挂起（Suspend-on-Idle）
)

// AgentTrigger 定义驱动 FSM 状态转移的触发器枚举。
type AgentTrigger int

const (
	TriggerIntentReceived AgentTrigger = iota
	TriggerPerceiveDone
	TriggerPlanDone
	TriggerValidateOk
	TriggerValidateFail
	TriggerExecuteDone
	TriggerExecuteFail
	TriggerReflectDone
	TriggerRollbackDone
	TriggerRollbackPartial
	TriggerReplanDone
	TriggerReplanExhausted
	TriggerInterruptReceived // 用户中断信号（任意活跃态均可接收，inv_global_08）
	TriggerInterruptResume   // 中断后恢复执行（回到 interruptFrom 状态）
	TriggerInterruptAbort    // 中断后终止任务 → S_FAILED
	TriggerSuspend           // Suspend-on-Idle 触发挂起
	TriggerResume            // 从挂起状态恢复 → S_IDLE
)

// InterruptAction 定义中断处理语义。
// 来源: internal/protocol/interfaces.go §Agent 控制接口
type InterruptAction int

const (
	InterruptResume   InterruptAction = iota // 恢复执行（回到被中断的状态）
	InterruptRedirect                        // 重新规划（新意图 → S_PERCEIVE）
	InterruptAbort                           // 终止任务 → S_FAILED
)
