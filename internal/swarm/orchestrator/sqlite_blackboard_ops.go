package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// RenewLease 续约（重置 expires_at = now + DefaultLeaseTTL）。
func (bb *SQLiteBlackboard) RenewLease(ctx context.Context, taskID, agentID string) error {
	expiresAt := time.Now().Add(DefaultLeaseTTL).UTC().Format(time.RFC3339)
	res, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET expires_at=?, updated_at=datetime('now'), version=version+1
		WHERE task_id=? AND claimed_by=? AND status=?`,
		expiresAt, taskID, agentID, statusClaimed,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.RenewLease", err)
	}
	rows, raErr := res.RowsAffected()
	if raErr != nil {
		slog.Warn("blackboard: RowsAffected failed", "err", raErr)
	}
	if rows == 0 {
		return ErrStaleBlackboardLease
	}
	return nil
}

// SideEffectPreCheck TOCTOU 校验。
func (bb *SQLiteBlackboard) SideEffectPreCheck(ctx context.Context, taskID, agentID string, claimedVersion int32) error {
	var status string
	var claimedBy sql.NullString
	var expiresAtStr string
	var version int32

	err := bb.db.QueryRowContext(ctx, `
		SELECT status, claimed_by, expires_at, version FROM tasks WHERE task_id=?`,
		taskID,
	).Scan(&status, &claimedBy, &expiresAtStr, &version)

	if err != nil {
		if err == sql.ErrNoRows {
			return ErrTaskNotOwned // using existing error
		}
		return apperr.Wrap(apperr.CodeInternal, "blackboard.SideEffectPreCheck", err)
	}

	if !claimedBy.Valid || claimedBy.String != agentID {
		return ErrStaleBlackboardLease
	}

	if version != claimedVersion {
		return ErrStaleBlackboardLease
	}

	if status != statusRunning {
		return ErrStaleBlackboardLease
	}

	expiresAt, _ := time.Parse(time.RFC3339, expiresAtStr)
	if time.Now().UTC().After(expiresAt) {
		return ErrStaleBlackboardLease
	}

	return nil
}

// PeekTask 只读快照提取。
func (bb *SQLiteBlackboard) PeekTask(ctx context.Context, taskID string) (*types.TaskSnapshot, error) {
	var statusStr string
	var namespace sql.NullString
	err := bb.db.QueryRowContext(ctx, "SELECT status, namespace FROM tasks WHERE task_id=?", taskID).Scan(&statusStr, &namespace)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "blackboard.PeekTask", err)
	}

	var status types.TaskStatus
	switch statusStr {
	case statusPending:
		status = types.TaskPending
	case statusClaimed:
		status = types.TaskClaimed
	case statusRunning:
		status = types.TaskExecuting
	case statusDone:
		status = types.TaskDone
	case statusFailed:
		status = types.TaskFailed
	case statusSuspended:
		status = types.TaskSuspended
	case "compensating":
		status = types.TaskCompensating
	}

	return &types.TaskSnapshot{
		ID:        taskID,
		Status:    status,
		Namespace: namespace.String,
	}, nil
}

// Subscribe 返回事件订阅通道（chan cap=64，背压丢弃策略）。
// 调用方须在 context 取消后不再读取通道。
func (bb *SQLiteBlackboard) Subscribe(ctx context.Context) (<-chan types.BlackboardEvent, error) {
	ch := make(chan types.BlackboardEvent, 64)
	bb.subMu.Lock()
	bb.subscribers = append(bb.subscribers, ch)
	bb.subMu.Unlock()

	// ctx 取消时自动注销
	concurrent.SafeGo(ctx, "swarm.blackboard_ops_async", func(ctx context.Context) {
		<-ctx.Done()
		bb.subMu.Lock()
		defer bb.subMu.Unlock()
		for i, s := range bb.subscribers {
			if s == ch {
				bb.subscribers = append(bb.subscribers[:i], bb.subscribers[i+1:]...)
				close(ch)
				break
			}
		}
	})
	return ch, nil
}

const (
	ZombieTaskTTL = 5 * time.Minute
	StarvationTTL = 30 * time.Minute
	DoneTaskTTL   = 5 * time.Minute
)

// StopAll 将所有 claimed/running 任务标记为 suspended 并广播 killswitch_stopall 事件，
// 供 KillSwitch 触发时的紧急停止使用。
func (bb *SQLiteBlackboard) StopAll(ctx context.Context, reason string) error {
	_, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET status=?, suspend_reason=?, version=version+1, updated_at=datetime('now')
		WHERE status IN (?, ?)`,
		statusSuspended, reason, statusClaimed, statusRunning,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.StopAll", err)
	}
	bb.broadcast(types.BlackboardEvent{Type: "killswitch_stopall", Payload: []byte(reason)})
	return nil
}

// UpdateTaskTokens 写入任务的 token 消耗（Gap-A, HE-Rule-1）。
// SQL 覆盖写入（幂等），任务不存在时静默成功（已被 Reaper 清理）。
func (bb *SQLiteBlackboard) UpdateTaskTokens(ctx context.Context, taskID string, tokensIn, tokensOut, cacheRead int, costUSD float64) error {
	_, err := bb.db.ExecContext(ctx, `
		UPDATE tasks
		SET tokens_input=?, tokens_output=?, tokens_cache_read=?, cost_usd=?, updated_at=datetime('now')
		WHERE task_id=?`,
		tokensIn, tokensOut, cacheRead, costUSD, taskID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.UpdateTaskTokens", err)
	}
	return nil
}

// broadcast 广播事件到所有订阅通道（非阻塞，背压丢弃）。
func (bb *SQLiteBlackboard) broadcast(ev types.BlackboardEvent) {
	bb.subMu.RLock()
	defer bb.subMu.RUnlock()
	for _, ch := range bb.subscribers {
		select {
		case ch <- ev:
		default:
			// 背压丢弃：消费者太慢时丢弃最新事件（保护 blackboard 不被阻塞）
		}
	}
}

// ─── 错误类型 ────────────────────────────────────────────────────────────────

var (
	ErrTaskNotOwned         = apperr.New(apperr.CodeInternal, "blackboard: task not owned by this agent or in wrong state")
	ErrStaleBlackboardLease = apperr.New(apperr.CodeInternal, "blackboard: lease expired or task not claimed by this agent")
)

// Ping 检测数据库连接是否存活（健康检查用）。
func (bb *SQLiteBlackboard) Ping(ctx context.Context) error {
	if err := bb.db.PingContext(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "blackboard.Ping", err)
	}
	return nil
}

// CountByStatus 返回处于任一给定状态的任务数（活跃度信号，只读）。
func (bb *SQLiteBlackboard) CountByStatus(statuses ...types.TaskStatus) int {
	if len(statuses) == 0 {
		return 0
	}
	// 将 types.TaskStatus 转为 SQL 存储字符串（与 status* 常量一致）
	args := make([]any, 0, len(statuses))
	for _, s := range statuses {
		var sqlStatus string
		switch s {
		case types.TaskPending:
			sqlStatus = statusPending
		case types.TaskClaimed:
			sqlStatus = statusClaimed
		case types.TaskExecuting:
			sqlStatus = statusRunning
		case types.TaskSuspended:
			sqlStatus = statusSuspended
		case types.TaskDone:
			sqlStatus = statusDone
		case types.TaskFailed:
			sqlStatus = statusFailed
		default:
			sqlStatus = fmt.Sprintf("%d", s)
		}
		args = append(args, sqlStatus)
	}
	placeholders := "?" + strings.Repeat(",?", len(args)-1)
	var count int
	if err := bb.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM tasks WHERE status IN ("+placeholders+")",
		args...).Scan(&count); err != nil {
		return 0
	}
	return count
}

// MaxActivePriority 返回活跃任务（Claimed/Executing）的最高优先级（0=最高）。
// 无活跃任务返回 3（最低优先级 → 认知压力低）。
func (bb *SQLiteBlackboard) MaxActivePriority() int {
	var minPrio sql.NullInt32
	err := bb.db.QueryRowContext(context.Background(),
		"SELECT MIN(priority) FROM tasks WHERE status IN (?, ?)",
		statusClaimed, statusRunning).Scan(&minPrio)
	if err != nil || !minPrio.Valid {
		return 3 // 无活跃任务 → 最低优先级权重(0.1) → 认知压力低
	}
	return int(minPrio.Int32)
}
