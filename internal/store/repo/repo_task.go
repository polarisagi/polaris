package repo

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
)

// SQLiteTaskReadRepository 实现 protocol.TaskReadRepository。
// 只读读取 tasks 表和 events 表（写路径由 Blackboard CAS 持有 *sql.DB）。
// @arch: docs/arch/M02-Storage-Fabric.md
type SQLiteTaskReadRepository struct {
	db *sql.DB
}

var _ protocol.TaskReadRepository = (*SQLiteTaskReadRepository)(nil)

// NewSQLiteTaskReadRepository 创建 SQLiteTaskReadRepository。
func NewSQLiteTaskReadRepository(db *sql.DB) *SQLiteTaskReadRepository {
	return &SQLiteTaskReadRepository{db: db}
}

// GetTaskProviderSuspendCount 返回指定任务的 provider_suspended_count。
func (r *SQLiteTaskReadRepository) GetTaskProviderSuspendCount(ctx context.Context, taskID string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT provider_suspended_count FROM tasks WHERE task_id = ?`, taskID,
	).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskReadRepository.GetTaskProviderSuspendCount", err)
	}
	return count, nil
}

// GetTaskIntentTaint 返回指定任务的 intent_taint。
func (r *SQLiteTaskReadRepository) GetTaskIntentTaint(ctx context.Context, taskID string) (int, error) {
	var taint int
	err := r.db.QueryRowContext(ctx,
		`SELECT intent_taint FROM tasks WHERE task_id = ?`, taskID,
	).Scan(&taint)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskReadRepository.GetTaskIntentTaint", err)
	}
	return taint, nil
}
