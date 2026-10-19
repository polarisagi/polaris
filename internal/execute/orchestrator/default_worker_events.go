package orchestrator

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/pkg/types"
)

func (w *DefaultTaskWorker) handleTaskEvent(ev types.AgentStreamEvent, taskID string, cancel context.CancelFunc) {
	if ev.Type == types.AgentStreamEventSteerAccepted {
		w.handleSteerEvent(taskID, ev.Content)
		return
	}
	if ev.Type == types.AgentStreamEventStatus && ev.Content == "cancel" {
		cancel()
		w.handleCancelEvent(taskID)
	}
}

func (w *DefaultTaskWorker) handleSteerEvent(taskID, content string) {
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

func (w *DefaultTaskWorker) handleCancelEvent(taskID string) {
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
