package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type SQLiteCronRepository struct {
	db *sql.DB
}

var _ protocol.CronRepository = (*SQLiteCronRepository)(nil)

func NewSQLiteCronRepository(db *sql.DB) *SQLiteCronRepository {
	return &SQLiteCronRepository{db: db}
}

// ListCronJobs 列出所有 cron job
func (r *SQLiteCronRepository) ListCronJobs(ctx context.Context) ([]types.CronJobRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, prompt, schedule, session_id, enabled, last_run_at, next_run_at, failure_count, circuit_open, last_error, circuit_opened_at, created_at
		FROM cron_jobs ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.ListCronJobs", err)
	}
	defer rows.Close()

	var result []types.CronJobRow
	for rows.Next() {
		var row types.CronJobRow
		var enabledInt, circuitOpenInt int
		if err := rows.Scan(&row.ID, &row.Name, &row.Prompt, &row.Schedule, &row.SessionID, &enabledInt, &row.LastRunAt, &row.NextRunAt, &row.FailureCount, &circuitOpenInt, &row.LastError, &row.CircuitOpenedAt, &row.CreatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.ListCronJobs scan", err)
		}
		row.Enabled = enabledInt == 1
		row.CircuitOpen = circuitOpenInt == 1
		result = append(result, row)
	}
	return result, rows.Err()
}

// GetCronJob 获取单个 cron job
func (r *SQLiteCronRepository) GetCronJob(ctx context.Context, id string) (*types.CronJobRow, error) {
	var row types.CronJobRow
	var enabledInt, circuitOpenInt int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, prompt, schedule, session_id, enabled, last_run_at, next_run_at, failure_count, circuit_open, last_error, circuit_opened_at, created_at
		FROM cron_jobs WHERE id=?`, id).Scan(&row.ID, &row.Name, &row.Prompt, &row.Schedule, &row.SessionID, &enabledInt, &row.LastRunAt, &row.NextRunAt, &row.FailureCount, &circuitOpenInt, &row.LastError, &row.CircuitOpenedAt, &row.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.GetCronJob", err)
	}
	row.Enabled = enabledInt == 1
	row.CircuitOpen = circuitOpenInt == 1
	return &row, nil
}

// CreateCronJob 创建一个 cron job
func (r *SQLiteCronRepository) CreateCronJob(ctx context.Context, row types.CronJobRow) error {
	enabledInt := 0
	if row.Enabled {
		enabledInt = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO cron_jobs(id, name, prompt, schedule, session_id, enabled, created_at)
		VALUES(?,?,?,?,?,?,?)`,
		row.ID, row.Name, row.Prompt, row.Schedule, row.SessionID, enabledInt, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.CreateCronJob", err)
	}
	return nil
}

// UpdateCronJob 更新一个 cron job
func (r *SQLiteCronRepository) UpdateCronJob(ctx context.Context, row types.CronJobRow) error {
	enabledInt := 0
	if row.Enabled {
		enabledInt = 1
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE cron_jobs SET name=?, prompt=?, schedule=?, session_id=?, enabled=? WHERE id=?`,
		row.Name, row.Prompt, row.Schedule, row.SessionID, enabledInt, row.ID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.UpdateCronJob", err)
	}
	return nil
}

// DeleteCronJob 删除 cron job
func (r *SQLiteCronRepository) DeleteCronJob(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.DeleteCronJob", err)
	}
	return nil
}

// UpdateCircuitBreaker 更新断路器状态
func (r *SQLiteCronRepository) UpdateCircuitBreaker(ctx context.Context, id string, failureCount int, circuitOpen bool, lastError, circuitOpenedAt string) error {
	circuitOpenInt := 0
	if circuitOpen {
		circuitOpenInt = 1
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE cron_jobs SET failure_count=?, circuit_open=?, last_error=?, circuit_opened_at=? WHERE id=?`,
		failureCount, circuitOpenInt, lastError, circuitOpenedAt, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.UpdateCircuitBreaker", err)
	}
	return nil
}

// UpdateLastRun 更新运行时间
func (r *SQLiteCronRepository) UpdateLastRun(ctx context.Context, id, lastRunAt, nextRunAt string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE cron_jobs SET last_run_at=?, next_run_at=? WHERE id=?`,
		lastRunAt, nextRunAt, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteCronRepository.UpdateLastRun", err)
	}
	return nil
}
