package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AwaitingHandoffReconciler 在进程重启后恢复处于 S_AWAIT_AGENT 态的会话。
// 数据已由 agent_execute_effect_helpers.go 落盘到 task_checkpoints，
// 本组件仅负责「读取 + 重挂 watcher」，不重复 PostTask。
type AwaitingHandoffReconciler struct {
	checkpointRepo protocol.TaskCheckpointRepository
	pool           protocol.AgentPool
	handoffPoster  HandoffPoster
	scanInterval   time.Duration
}

func NewAwaitingHandoffReconciler(
	checkpointRepo protocol.TaskCheckpointRepository,
	pool protocol.AgentPool,
	handoffPoster HandoffPoster,
) *AwaitingHandoffReconciler {
	return &AwaitingHandoffReconciler{
		checkpointRepo: checkpointRepo,
		pool:           pool,
		handoffPoster:  handoffPoster,
		scanInterval:   5 * time.Minute,
	}
}

// Reconcile 扫描并恢复所有处于 await_agent 状态的会话。
func (r *AwaitingHandoffReconciler) Reconcile(ctx context.Context) error {
	rows, err := r.checkpointRepo.ListByStatus(ctx, "await_agent")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "AwaitingHandoffReconciler: list checkpoints failed", err)
	}
	for _, row := range rows {
		if row.Reason != "handoff_wait" {
			continue
		}
		sessionID := row.TaskID
		childTaskID := row.NodeID

		// 尝试获取 Agent 实例，如果不存在则跳过（不强制创建）
		ctrl, release, err := r.pool.Acquire(ctx, sessionID)
		if err != nil {
			slog.WarnContext(ctx, "AwaitingHandoffReconciler: acquire agent failed, skipping",
				"session_id", sessionID, "err", err)
			continue
		}

		ag, ok := ctrl.(*Agent)
		if !ok {
			release()
			slog.WarnContext(ctx, "AwaitingHandoffReconciler: agent is not of type *Agent", "session_id", sessionID)
			continue
		}

		// 查找子任务状态
		task, err := r.handoffPoster.PeekTask(ctx, childTaskID)
		if err != nil {
			release()
			slog.WarnContext(ctx, "AwaitingHandoffReconciler: peek task failed",
				"child_task_id", childTaskID, "err", err)
			continue
		}
		if task != nil && (task.Status == types.TaskDone || task.Status == types.TaskFailed) {
			// 子任务已完成，直接可以唤醒 FSM
			if err := ctrl.SendIntent(types.TriggerAgentHandoffDone); err != nil {
				slog.ErrorContext(ctx, "AwaitingHandoffReconciler: trigger handoff done failed",
					"session_id", sessionID, "err", err)
			}
			release()
		} else {
			// 子任务仍在运行，重新挂载 watcher
			slog.InfoContext(ctx, "AwaitingHandoffReconciler: re-attaching watcher",
				"session_id", sessionID, "child_task_id", childTaskID)
			ag.watchHandoffCompletion(childTaskID)
			release()
		}
	}
	return nil
}
