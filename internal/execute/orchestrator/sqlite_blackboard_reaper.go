package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	"golang.org/x/sync/errgroup"
)

// reaperPhase2 清理长时间未完成的僵尸任务（running/pending 超时）以及终态物理回收。
// 对 running 任务先 cancel（通过 bb.cancels），再标记 failed；
// 对 pending 超时任务直接标记 failed（防止饥饿任务永久堆积）。
//
//nolint:gocyclo,nestif
func (bb *SQLiteBlackboard) reaperPhase2(ctx context.Context) {
	ttlStr := fmt.Sprintf("-%d minute", int(bb.taskRetentionTTL.Minutes()))
	if bb.taskRetentionTTL == 0 {
		ttlStr = "-1440 minute" // default fallback
	}

	// 0. GD-13-004：终态任务归档 + 物理回收，必须在**同一事务**内完成。
	//
	// 归档与删除若各自独立执行，两者之间任一失败都会产生持久损坏：
	//   - 归档成功、删除失败 → 下一轮（30s 后）扫到同一批任务再归档一遍，
	//     DB 持续异常期间 decision_log 会被同一批 task_id 无界刷屏；
	//   - 归档失败、删除成功 → 任务历史彻底消失，正是 GD-13-004 要修的问题。
	// 事务保证"要么都发生、要么都不发生"，失败整体回滚交给下一轮重试。
	bb.archiveAndPurgeTerminalTasks(ctx, ttlStr)

	// 1. 取消 running 中的超时任务
	rows, err := bb.db.QueryContext(ctx,
		`SELECT task_id FROM tasks WHERE status='running' AND updated_at < datetime('now', '-30 minute')`)
	if err != nil {
		slog.WarnContext(ctx, "reaper phase2: query running failed", "error", err)
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		// L1（读上下文降级为同函数一致的 Warn+continue 语义，见提交信息说明）：
		// 扫描失败绝不能把零值 id 当作真实 task_id 追加进 ids——那会污染
		// bb.cancels 查找与后续批量 UPDATE 的 WHERE 子句（虽然空字符串不会误伤
		// 真实任务，但会让这一行僵尸任务本轮被跳过）。必须 continue 而非带着
		// 空值继续。
		if err := rows.Scan(&id); err != nil {
			slog.WarnContext(ctx, "blackboard: reaper phase2 running-zombie row scan failed, skipping this row", "err", err)
			metrics.RecordBlackboardScanError(ctx, "reaperPhase2_running_scan")
			continue
		}
		ids = append(ids, id)
	}
	_ = rows.Close() //nolint:errcheck // 已有 defer rows.Close() 兜底，此处显式二次调用（sql.Rows.Close 幂等安全）

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
			tx, err := bb.db.BeginTx(ctx, nil)
			if err != nil {
				continue
			}
			res, err := tx.ExecContext(ctx,
				`UPDATE tasks SET status='failed', error='reaper_phase2_zombie_timeout', updated_at=datetime('now') WHERE task_id=? AND status='running'`,
				id)
			if err != nil {
				_ = tx.Rollback()
				slog.WarnContext(ctx, "blackboard: zombie task status update failed", "task_id", id, "error", err)
				continue
			}
			ra, _ := res.RowsAffected()
			if ra > 0 {
				if werr := bb.writeTaskEvent(ctx, tx, "system:blackboard", "task_failed", id); werr != nil {
					slog.WarnContext(ctx, "blackboard: failed to write reaper event", "task_id", id, "error", werr)
				}
			}
			// 全量扫描补录（同函数内与已定级的 GR-6-003 同类问题，一并修复）：
			// commit 之前 broadcast 会导致"广播已发生但 DB 未必真的落盘"的不一致——
			// 若 Commit 失败，订阅者已经收到 task_failed 事件，但 DB 里该任务可能
			// 仍是 running。改为先 Commit 确认成功，再广播；Commit 失败则 Warn+
			// counter，交给下一轮 reaperPhase2（30s 后）重新捕获同一僵尸任务。
			if commitErr := tx.Commit(); commitErr != nil {
				slog.WarnContext(ctx, "blackboard: zombie task commit failed, will retry next scan", "task_id", id, "error", commitErr)
				metrics.RecordBlackboardScanError(ctx, "reaperPhase2_zombie_commit")
				continue
			}
			if ra > 0 {
				bb.broadcast(types.BlackboardEvent{Type: "task_failed", TaskID: id, Err: apperr.New(apperr.CodeTimeout, "reaper_phase2_zombie_timeout")})
			}
			slog.WarnContext(ctx, "reaper phase2: zombie task killed", "task_id", id)
		}
	}

	// 2. pending 超时（饥饿）任务
	tx, err := bb.db.BeginTx(ctx, nil)
	if err == nil {
		rows, err := tx.QueryContext(ctx,
			`UPDATE tasks SET status='failed', error='reaper_phase2_pending_timeout', updated_at=datetime('now')
         WHERE status='pending' AND created_at < datetime('now', '-60 minute') RETURNING task_id`)
		if err != nil {
			_ = tx.Rollback()
			slog.WarnContext(ctx, "blackboard: starvation cleanup failed", "error", err)
		} else {
			var starvedIDs []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					starvedIDs = append(starvedIDs, id)
				}
			}
			_ = rows.Close()
			for _, id := range starvedIDs {
				if werr := bb.writeTaskEvent(ctx, tx, "system:blackboard", "task_failed", id); werr != nil {
					slog.WarnContext(ctx, "blackboard: failed to write reaper event", "task_id", id, "error", werr)
				}
			}
			// 同上：先 Commit 确认成功，再广播，避免"广播已发生但 DB 未落盘"。
			if commitErr := tx.Commit(); commitErr != nil {
				slog.WarnContext(ctx, "blackboard: starvation cleanup commit failed, will retry next scan", "count", len(starvedIDs), "error", commitErr)
				metrics.RecordBlackboardScanError(ctx, "reaperPhase2_starvation_commit")
			} else {
				for _, id := range starvedIDs {
					bb.broadcast(types.BlackboardEvent{Type: "task_failed", TaskID: id, Err: apperr.New(apperr.CodeTimeout, "reaper_phase2_starvation_timeout")})
				}
			}
		}
	} else {
		slog.WarnContext(ctx, "blackboard: starvation tx begin failed", "error", err)
	}
}

// archiveAndPurgeTerminalTasks 在单个事务内归档并物理删除超出保留期的终态任务
// （GD-13-004）。
//
// 保留期语义：重构前是硬编码 5 分钟，对长程工作流/多 Agent 协同过短——
// `GET /v1/tasks/{id}` 在任务完成 5 分钟后即 404，崩溃诊断也拿不到失败原因
// （违反 HE-1 可观测优先）。现由 bb.taskRetentionTTL 控制（默认 24h，
// 见 configs/defaults.toml [orchestrator] task_retention_ttl）。
//
// 归档目标是 decision_log 而非新建审计表：该表本就是 append-only 的决策审计
// 单一入口（006_decision_log.sql），复用它避免再引入一张需要自己管清理的表。
func (bb *SQLiteBlackboard) archiveAndPurgeTerminalTasks(ctx context.Context, ttlStr string) {
	const terminalFilter = `status IN ('done', 'failed') AND updated_at < datetime('now', ?)`

	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		slog.WarnContext(ctx, "blackboard: reaper archive/purge tx begin failed, will retry next scan", "err", err)
		metrics.RecordBlackboardScanError(ctx, "reaperPhase2_archive_tx_begin")
		return
	}

	if _, archiveErr := tx.ExecContext(ctx, `
		INSERT INTO decision_log (timestamp, session_id, agent_id, decision_type, choice, context)
		SELECT
			CAST(strftime('%s', 'now') * 1000 AS INTEGER),
			'blackboard_reaper',
			'system',
			'task_archived',
			task_id,
			json_object(
				'task_id', task_id,
				'session_id', session_id,
				'status', status,
				'error', error,
				'created_at', created_at,
				'updated_at', updated_at
			)
		FROM tasks
		WHERE `+terminalFilter, ttlStr); archiveErr != nil {
		_ = tx.Rollback()
		// 归档失败必须连删除一起回滚：宁可让终态任务多留一轮（30s），
		// 也不能出现"删了但没归档"的历史断崖。
		slog.WarnContext(ctx, "blackboard: archive before delete failed, purge rolled back", "err", archiveErr)
		metrics.RecordBlackboardScanError(ctx, "reaperPhase2_archive")
		return
	}

	result, delErr := tx.ExecContext(ctx, `DELETE FROM tasks WHERE `+terminalFilter, ttlStr)
	if delErr != nil {
		_ = tx.Rollback()
		slog.WarnContext(ctx, "blackboard: reaper cleanup failed, archive rolled back", "error", delErr)
		metrics.RecordBlackboardScanError(ctx, "reaperPhase2_purge")
		return
	}
	affected, _ := result.RowsAffected()

	if commitErr := tx.Commit(); commitErr != nil {
		slog.WarnContext(ctx, "blackboard: reaper archive/purge commit failed, will retry next scan", "error", commitErr)
		metrics.RecordBlackboardScanError(ctx, "reaperPhase2_archive_commit")
		return
	}
	if affected > 0 {
		slog.InfoContext(ctx, "blackboard: reaper phase2 cleanup", "deleted", affected, "retention", ttlStr)
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

	// L2：批量回写失败下一轮 reap（1s 后）仍会重新扫到同一批 expires_at 已过期
	// 的任务并重试，不阻断（cancel() 已经执行，goroutine 已经中止，只是 DB
	// 状态未及时回写），但必须 Warn + counter，持续失败意味着 DB 层面有问题。
	if _, err := bb.db.ExecContext(ctx, query, args...); err != nil {
		slog.WarnContext(ctx, "blackboard: reap lease-expire batch update failed, will retry next scan", "count", len(expired), "err", err)
		metrics.RecordBlackboardScanError(ctx, "reap_lease_expire_update")
	}

	for _, r := range expired {
		bb.broadcast(types.BlackboardEvent{
			Type:    "task_lease_expired",
			TaskID:  r.taskID,
			AgentID: r.agentID,
		})
	}
}

// StopAll KillSwitch FullStop 响应：所有 Executing 任务进入 Suspended(oom_evicted)。
