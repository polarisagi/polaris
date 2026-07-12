package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"golang.org/x/sync/errgroup"
)

// reaperPhase2 清理长时间未完成的僵尸任务（running/pending 超时）以及终态物理回收。
// 对 running 任务先 cancel（通过 bb.cancels），再标记 failed；
// 对 pending 超时任务直接标记 failed（防止饥饿任务永久堆积）。
func (bb *SQLiteBlackboard) reaperPhase2(ctx context.Context) {
	zombieCutoff := time.Now().Add(-ZombieTaskTTL).UnixMilli()
	starvationCutoff := time.Now().Add(-StarvationTTL).UnixMilli()

	// 0. 物理删除终态任务（保留原有物理清理逻辑）
	if _, err := bb.db.ExecContext(ctx, `
		DELETE FROM tasks
		WHERE status IN ('done', 'failed') AND updated_at < datetime('now', '-5 minute')
	`); err != nil {
		slog.WarnContext(ctx, "blackboard: reaper cleanup failed", "error", err)
	}

	// 1. 取消 running 中的超时任务
	rows, err := bb.db.QueryContext(ctx,
		`SELECT task_id FROM tasks WHERE status='running' AND updated_at < ?`, zombieCutoff)
	if err != nil {
		slog.WarnContext(ctx, "reaper phase2: query running failed", "error", err)
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	_ = rows.Close()

	bb.mu.Lock()
	for _, id := range ids {
		if cancel, ok := bb.cancels[id]; ok {
			cancel()
			delete(bb.cancels, id)
		}
	}
	bb.mu.Unlock()

	if len(ids) > 0 {
		// 批量标记 failed
		for _, id := range ids {
			if _, err := bb.db.ExecContext(ctx,
				`UPDATE tasks SET status='failed', error='reaper_phase2_zombie_timeout', updated_at=? WHERE task_id=? AND status='running'`,
				time.Now().UnixMilli(), id); err != nil {
				slog.WarnContext(ctx, "blackboard: zombie task status update failed", "task_id", id, "error", err)
			}
			slog.WarnContext(ctx, "reaper phase2: zombie task killed", "task_id", id)
		}
	}

	// 2. pending 超时（饥饿）任务
	if _, err := bb.db.ExecContext(ctx,
		`UPDATE tasks SET status='failed', error='reaper_phase2_pending_timeout', updated_at=?
         WHERE status='pending' AND created_at < ?`,
		time.Now().UnixMilli(), starvationCutoff); err != nil {
		slog.WarnContext(ctx, "blackboard: starvation cleanup failed", "error", err)
	}
}

// reap 扫描 expires_at 已过期的 claimed 任务。
// 1. 并发调用所有过期任务的 cancel() 触发协程中止。
// 2. 等待 5s 宽限期（供 M7 工具感知 ctx.Done() 并完成清理）。
// 3. 宽限期结束后强制更新 DB：Status=Pending, Version++。
func (bb *SQLiteBlackboard) reap(ctx context.Context) {
	rows, err := bb.db.QueryContext(ctx, `
		SELECT task_id, claimed_by FROM tasks
		WHERE status IN (?,?) AND expires_at < datetime('now')`,
		statusClaimed, statusRunning,
	)
	if err != nil {
		return
	}

	type row struct{ taskID, agentID string }
	var expired []row

	for rows.Next() {
		var r row
		if rows.Scan(&r.taskID, &r.agentID) == nil {
			expired = append(expired, r)
		}
	}
	rows.Close()

	if len(expired) == 0 {
		return
	}

	var toCancel []context.CancelFunc
	bb.mu.Lock()
	for _, r := range expired {
		if cancel, ok := bb.cancels[r.taskID]; ok && cancel != nil {
			toCancel = append(toCancel, cancel)
			delete(bb.cancels, r.taskID)
		}
	}
	bb.mu.Unlock()

	// 并发 cancel
	var eg errgroup.Group
	for _, cancel := range toCancel {
		c := cancel
		eg.Go(func() error {
			c()
			return nil
		})
	}
	_ = eg.Wait()

	// 宽限期：给 M7 工具的 ctx.Done() 感知路径留出 5s 时间窗口
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return
	}

	// 宽限期结束，强制回写 DB（批量 UPDATE）
	var taskIDs = make([]any, 0, len(expired))
	var placeholders = make([]string, 0, len(expired))
	for _, r := range expired {
		taskIDs = append(taskIDs, r.taskID)
		placeholders = append(placeholders, "?")
	}

	query := fmt.Sprintf(`
		UPDATE tasks
		SET status = CASE WHEN retry_count + 1 >= max_retries THEN ? ELSE ? END,
		    claimed_by=NULL, claimed_at=NULL, expires_at=NULL,
		    provider_suspended_count=provider_suspended_count+1,
		    retry_count=retry_count+1,
		    version=version+1, updated_at=datetime('now')
		WHERE status IN (?,?) AND task_id IN (%s)`, strings.Join(placeholders, ","))

	args := []any{statusFailed, statusPending, statusClaimed, statusRunning} //nolint:prealloc
	args = append(args, taskIDs...)

	_, _ = bb.db.ExecContext(ctx, query, args...)

	for _, r := range expired {
		bb.broadcast(types.BlackboardEvent{
			Type:    "task_lease_expired",
			TaskID:  r.taskID,
			AgentID: r.agentID,
		})
	}
}

// StopAll KillSwitch FullStop 响应：所有 Executing 任务进入 Suspended(oom_evicted)。
