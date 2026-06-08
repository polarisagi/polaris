package patterns

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/swarm"
)

// ParallelExecutor 实现了并发编排模式。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3
// 行为: 将多个无依赖的子任务同时投递到黑板，并等待它们全部完成。
type ParallelExecutor struct {
	bb *swarm.SQLiteBlackboard
}

func NewParallelExecutor(bb *swarm.SQLiteBlackboard) *ParallelExecutor {
	return &ParallelExecutor{bb: bb}
}

// Execute 批量投递任务并使用 errgroup 等待它们完成。
func (pe *ParallelExecutor) Execute(ctx context.Context, parentTaskID string, subTasks []protocol.TaskEntry) error {
	if len(subTasks) == 0 {
		return nil
	}

	// 1. 先订阅事件，再投递任务，防止错过完成通知
	events, err := pe.bb.Subscribe(ctx)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "failed to subscribe to blackboard", err)
	}

	for i := range subTasks {
		task := &subTasks[i]
		if err := pe.bb.PostTask(ctx, task); err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("failed to post parallel task %s", task.ID), err)
		}
	}

	// 3. 跟踪完成情况
	pendingMap := make(map[string]bool)
	for _, task := range subTasks {
		pendingMap[task.ID] = true
	}

	g, gCtx := errgroup.WithContext(ctx)

	// 启动一个监听协程
	g.Go(func() error {
		for len(pendingMap) > 0 {
			select {
			case <-gCtx.Done():
				return gCtx.Err()
			case ev, ok := <-events:
				if !ok {
					return perrors.New(perrors.CodeInternal, fmt.Sprintf("event channel closed while %d tasks pending", len(pendingMap)))
				}
				if ev.TaskID != "" && pendingMap[ev.TaskID] {
					if ev.Type == "task_completed" {
						delete(pendingMap, ev.TaskID)
					} else if ev.Type == "task_failed" {
						return perrors.New(perrors.CodeInternal, fmt.Sprintf("parallel task %s failed with: %s", ev.TaskID, string(ev.Payload)))
					}
				}
			}
		}
		return nil
	})

	return g.Wait()
}
