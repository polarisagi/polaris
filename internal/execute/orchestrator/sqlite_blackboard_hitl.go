package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SuspendForHITL 将 Executing 的任务挂起（HITL超时戳覆盖ExpiresAt）。
func (bb *SQLiteBlackboard) SuspendForHITL(ctx context.Context, taskID, agentID string, timeout int64) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.SuspendForHITL: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	expiresAt := time.Unix(timeout, 0).UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		statusSuspended, expiresAt, taskID, agentID, statusRunning,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.SuspendForHITL", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return ErrTaskNotOwned
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, "task_suspended_hitl", taskID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.SuspendForHITL: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.SuspendForHITL: commit", err)
	}
	bb.broadcast(types.BlackboardEvent{Type: "task_suspended_hitl", TaskID: taskID, AgentID: agentID})
	return nil
}

// ResumeFromHITL 恢复被挂起的任务（!approved → Failed）。
func (bb *SQLiteBlackboard) ResumeFromHITL(ctx context.Context, taskID, agentID string, approved bool) error {
	tx, err := bb.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.ResumeFromHITL: begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	newStatus := statusRunning
	if !approved {
		newStatus = statusFailed
	}

	expiresAt := time.Now().Add(DefaultLeaseTTL).UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		newStatus, expiresAt, taskID, agentID, statusSuspended,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.ResumeFromHITL", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return ErrTaskNotOwned
	}
	evType := "task_resumed_hitl_approved"
	if !approved {
		evType = "task_resumed_hitl_rejected"
	}
	if err := bb.writeTaskEvent(ctx, tx, "agent:"+agentID, evType, taskID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.ResumeFromHITL: write event", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.ResumeFromHITL: commit", err)
	}
	bb.broadcast(types.BlackboardEvent{Type: evType, TaskID: taskID, AgentID: agentID})
	return nil
}

// BeginCompensation 开始补偿链（状态改为 compensating，提供 300s 时间预算）。
func (bb *SQLiteBlackboard) BeginCompensation(ctx context.Context, taskID, agentID string) error {
	expiresAt := time.Now().Add(300 * time.Second).UTC().Format(time.RFC3339)

	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET status='compensating', expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		expiresAt, taskID, agentID, statusRunning,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.BeginCompensation", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return ErrTaskNotOwned
	}
	return nil
}

// EndCompensation 补偿完成（状态改为 failed，进入正常回收）。
func (bb *SQLiteBlackboard) EndCompensation(ctx context.Context, taskID, agentID string) error {
	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status='compensating'`,
		statusFailed, taskID, agentID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.EndCompensation", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return ErrTaskNotOwned
	}
	return nil
}

// ResumeFromSuspended 将 suspended 任务重置为 pending 以便重新调度（幂等）。
func (bb *SQLiteBlackboard) ResumeFromSuspended(ctx context.Context, taskID string) error {
	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks SET status=?, claimed_by=NULL, claimed_at=NULL,
			expires_at=NULL, version=version+1, updated_at=datetime('now')
		WHERE task_id=? AND status=?`,
		statusPending, taskID, statusSuspended)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ResumeFromSuspended", err)
	}
	n, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if n == 0 {
		return apperr.New(apperr.CodeNotFound, "ResumeFromSuspended: no suspended task "+taskID)
	}
	return nil
}

// Ping 实现 Pinger 接口，P0 阶段 HealthCheckGate 使用。
