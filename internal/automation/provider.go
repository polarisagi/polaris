package automation

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 automation 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// automation 包（任务调度 + HITL）需要以下外部能力：
//   1. TaskDispatcher  — 将到期任务分发给 Agent 执行
//   2. TaskRepo        — 任务持久化（读写 tasks 表）
//   3. HITLRepo        — HITL 挂起状态持久化
//   4. BackgroundGate  — 资源管控门（KillSwitch / ResourceGovernor 联动）
//
// @consumer: automation/queue.go, automation/scheduler.go, automation/hitl/gateway.go
// @producer: 各具体模块由 cli.go/bootstrap 注入

// TaskDispatcher automation 包对 Agent 执行层的消费端接口。
// 实现：agent.Agent（通过 DependencyMap["AgentInvoker"] 注入）
// 禁止：automation 直接 import agent（防止循环依赖）
type TaskDispatcher interface {
	// Invoke 以 task.Payload 为输入触发 Agent 执行，返回执行结果摘要。
	Invoke(ctx context.Context, task *types.Task) (result []byte, err error)
}

// TaskRepo automation 包对任务持久化存储的消费端接口。
// 实现：store/repo.SQLiteTaskRepo
type TaskRepo interface {
	// Save 插入或更新任务记录（upsert by ID）。
	Save(ctx context.Context, task *types.Task) error
	// Load 按 ID 加载任务。
	Load(ctx context.Context, id string) (*types.Task, error)
	// ListReady 查询到期待执行的任务（run_at <= now, status=pending）。
	ListReady(ctx context.Context, limit int) ([]*types.Task, error)
	// UpdateStatus 更新任务状态（running/done/failed）及最后执行时间。
	UpdateStatus(ctx context.Context, id string, status types.TaskStatus) error
}

// HITLRepo automation/hitl 包对审批记录持久化的消费端接口。
// 实现：store/repo.SQLiteHITLRepo（若无专用表则复用 tasks 表附加字段）
type HITLRepo interface {
	// SavePrompt 保存 HITL 审批请求（status=pending）。
	SavePrompt(ctx context.Context, p types.HITLPrompt) error
	// LoadPrompt 按 checkpointID 加载审批请求。
	LoadPrompt(ctx context.Context, checkpointID string) (*types.HITLPrompt, error)
	// SaveResponse 保存审批决策（status=approved/rejected）。
	SaveResponse(ctx context.Context, checkpointID string, resp types.HITLResponse) error
	// ListPending 返回所有 status=pending 的审批请求。
	ListPending(ctx context.Context) ([]types.HITLPrompt, error)
}

// BackgroundGate automation 包对资源管控的消费端接口。
// 实现：security.KillSwitch（通过 DependencyMap["BackgroundGate"] 注入）
// 任务扫描循环在每次触发前调用 Allowed()，KillSwitch 进入 FullStop 时返回 false。
type BackgroundGate interface {
	// Allowed 返回当前系统是否允许后台任务运行。
	// false 表示 KillSwitch 已触发或系统资源不足。
	Allowed() bool
}
