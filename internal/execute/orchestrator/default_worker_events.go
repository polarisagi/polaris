package orchestrator

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// subscribeTaskControl 订阅独立控制通道（R3-04）。
// 控制通道只承载父 Agent 发出的指令（steer / cancel），与事件流完全分离，消除事件回环。
func (w *DefaultTaskWorker) subscribeTaskControl(ctx context.Context, taskID string, cancel context.CancelFunc) func() {
	if w.bb == nil {
		return nil
	}
	ctrlCh, cancelSub := w.bb.SubscribeTaskControl(taskID)
	concurrent.SafeGo(ctx, "default_worker.task_control_listener", func(runCtx context.Context) {
		for {
			select {
			case <-runCtx.Done():
				return
			case ctrl, ok := <-ctrlCh:
				if !ok {
					return
				}
				switch ctrl.Kind {
				case types.TaskControlSteer:
					w.handleSteer(taskID, ctrl.Text)
				case types.TaskControlCancel:
					cancel()
					w.handleCancel(taskID)
				}
			}
		}
	})
	return cancelSub
}

func (w *DefaultTaskWorker) handleSteer(taskID, content string) {
	steerable, ok := w.pool.(interface{ TrySteer(string, string) bool })
	if !ok {
		return
	}
	if steerable.TrySteer("sub-"+taskID, content) {
		return
	}
	if !steerable.TrySteer(taskID, content) {
		slog.Debug("default_worker: steer fallback missed", "task_id", taskID)
	}
}

func (w *DefaultTaskWorker) handleCancel(taskID string) {
	stoppable, ok := w.pool.(interface{ StopSession(string) bool })
	if !ok {
		return
	}
	if stoppable.StopSession("sub-" + taskID) {
		return
	}
	if !stoppable.StopSession(taskID) {
		slog.Debug("default_worker: stop session fallback missed", "task_id", taskID)
	}
}
