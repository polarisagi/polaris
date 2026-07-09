package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// AutomationFacade automation 包对外统一接口（任务调度 + HITL 审批）。
type AutomationFacade interface {
	// Submit 提交一个任务到调度队列。
	// 返回任务 ID（UUID），供后续 Get/Cancel/Subscribe 使用。
	Submit(ctx context.Context, task types.Task) (taskID string, err error)

	// Get 按 taskID 查询任务状态（含调度时间、重试次数等）。
	Get(ctx context.Context, taskID string) (*types.Task, error)

	// Cancel 取消一个待执行任务（已运行的任务不受影响）。
	Cancel(ctx context.Context, taskID string) error

	// Subscribe 订阅任务状态变更事件（ctx 取消时 channel 关闭）。
	// 每次状态变更（pending→running→done/failed）推送一次 TaskEvent。
	Subscribe(ctx context.Context, taskID string) (<-chan types.TaskEvent, error)

	// HITLPrompt 挂起当前执行并请求人工审批（ESCALATE 协议入口）。
	// 阻塞直到审批完成或 ctx 超时。
	HITLPrompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error)

	// HITLRespond 提交人工审批决策（由 gateway/server HITL API 调用）。
	HITLRespond(ctx context.Context, checkpointID string, resp types.HITLResponse) error

	// HITLPending 返回当前所有待审批请求（供 UI 展示）。
	HITLPending(ctx context.Context) ([]types.HITLPrompt, error)
}
