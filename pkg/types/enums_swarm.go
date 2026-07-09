package types

// ============================================================================
// M8 Multi-Agent Orchestrator — 任务状态枚举
// 来源: internal/protocol/types.go §M8
// 架构文档: docs/arch/08-Multi-Agent-Orchestrator-深度选型.md §1
//
// 黑板事件 & 出箱状态枚举
// 来源: internal/protocol/event.go
//
// 从 enums.go 按模块拆出（R7 文件行数治理，2026-07-07），纯类型/常量/String()
// 声明，无逻辑变更。
// ============================================================================

// TaskStatus 定义黑板任务的生命周期状态。
type TaskStatus int

const (
	TaskPending TaskStatus = iota
	TaskClaimed
	TaskExecuting
	TaskSuspended
	TaskCompensating
	TaskDone
	TaskFailed
)

// VerificationVerdict 流水线验证阶段的结论等级。
type VerificationVerdict int

const (
	VerdictPass    VerificationVerdict = iota // 目标已达成
	VerdictWarning                            // 有不确定项，人工决策
	VerdictBlocker                            // 目标未达成，必须重规划
)

// String 返回可读字符串，实现 fmt.Stringer（标准库要求，L0 例外）。
func (v VerificationVerdict) String() string {
	switch v {
	case VerdictPass:
		return "PASS"
	case VerdictWarning:
		return "WARNING"
	case VerdictBlocker:
		return "BLOCKER"
	default:
		return "UNKNOWN"
	}
}

// EventType 枚举黑板协调事件类型（跨模块走结构化事件，XR-04）。
type EventType string

const (
	EventIntent        EventType = "intent"
	EventClaim         EventType = "claim"
	EventResult        EventType = "result"
	EventFail          EventType = "fail"
	EventCancel        EventType = "cancel"
	EventHeartbeat     EventType = "heartbeat"
	EventActionPending EventType = "action_pending"
	EventActionDone    EventType = "action_done"

	// M9 自改善闭环事件
	EventHeuristicGenerated EventType = "heuristic_generated"
	EventEvalCompleted      EventType = "eval_completed"
)

// EventStatus 跟踪事件生命周期。
type EventStatus string

const (
	StatusPending   EventStatus = "pending"
	StatusClaimed   EventStatus = "claimed"
	StatusExecuting EventStatus = "executing"
	StatusDone      EventStatus = "done"
	StatusFailed    EventStatus = "failed"
)

// OutboxStatus 出箱记录状态。
type OutboxStatus string

const (
	OutboxPending    OutboxStatus = "pending"
	OutboxProcessing OutboxStatus = "processing"
	OutboxDone       OutboxStatus = "done"
	OutboxDead       OutboxStatus = "dead" // 超过 MaxRetries
)
