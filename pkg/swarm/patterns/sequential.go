package patterns

import (
	"context"
	"fmt"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/swarm"
)

// SequentialExecutor 实现了串行编排模式。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// 行为: Task A 的输出将作为 Task B 的输入，依次串联执行。
type SequentialExecutor struct {
	bb             *swarm.SQLiteBlackboard
	perTaskTimeout time.Duration // 单任务等待超时，默认 5min
}

// NewSequentialExecutor 创建 SequentialExecutor，perTaskTimeout=0 使用默认值 5min。
func NewSequentialExecutor(bb *swarm.SQLiteBlackboard, perTaskTimeout time.Duration) *SequentialExecutor {
	if perTaskTimeout <= 0 {
		perTaskTimeout = 5 * time.Minute
	}
	return &SequentialExecutor{bb: bb, perTaskTimeout: perTaskTimeout}
}

// Execute 依次投递任务，并等待上一个任务完成再投递下一个。
func (se *SequentialExecutor) Execute(ctx context.Context, parentTaskID string, subTasks []protocol.TaskEntry) error {
	var lastResult []byte

	for i := range subTasks {
		task := &subTasks[i]
		// 1. 如果不是第一个任务，将上一个任务的结果注入到当前任务的 Payload（这里以追加 Intent 为例）
		if i > 0 && len(lastResult) > 0 {
			task.Intent = append(task.Intent, []byte(fmt.Sprintf("\n[Previous Result]: %s", string(lastResult)))...)
		}

		// 2. 投递任务
		if err := se.bb.PostTask(ctx, task); err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("failed to post sequential task %s", task.ID), err)
		}

		// 3. 订阅黑板事件，等待当前任务完成
		events, err := se.bb.Subscribe(ctx)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, "failed to subscribe to blackboard", err)
		}

		completed := false
		for !completed {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ev := <-events:
				if ev.TaskID == task.ID {
					if ev.Type == "task_completed" {
						lastResult = ev.Payload
						completed = true
					} else if ev.Type == "task_failed" {
						return perrors.New(perrors.CodeInternal, fmt.Sprintf("sequential pipeline broken: task %s failed with: %s", task.ID, string(ev.Payload)))
					}
				}
			case <-time.After(se.perTaskTimeout):
				return perrors.New(perrors.CodeInternal, fmt.Sprintf("sequential pipeline timeout waiting for task %s", task.ID))
			}
		}
	}

	// 最终将最后一个任务的结果写回 Parent Task
	// 此处省略父任务的 CompleteTask 调用，交给上层 Orchestrator 或 Planner
	return nil
}
