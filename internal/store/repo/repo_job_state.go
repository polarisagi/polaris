package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type SQLiteBackgroundJobStateRepository struct {
	db protocol.SQLQuerier
}

var _ repo.BackgroundJobStateRepository = (*SQLiteBackgroundJobStateRepository)(nil)

func NewSQLiteBackgroundJobStateRepository(db protocol.SQLQuerier) *SQLiteBackgroundJobStateRepository {
	return &SQLiteBackgroundJobStateRepository{db: db}
}

func (r *SQLiteBackgroundJobStateRepository) GetLastRun(ctx context.Context, job string) (time.Time, bool, error) {
	var ms int64
	err := r.db.QueryRowContext(ctx,
		`SELECT last_run_at FROM background_job_state WHERE job_name = ?`, job).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return time.UnixMilli(ms), true, nil
}

func (r *SQLiteBackgroundJobStateRepository) RecordRun(ctx context.Context, job, status string, at time.Time) error {
	now := time.Now().UnixMilli()
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO background_job_state(job_name, last_run_at, last_status, updated_at)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(job_name) DO UPDATE SET
		   last_run_at=excluded.last_run_at, last_status=excluded.last_status, updated_at=excluded.updated_at`,
		job, at.UnixMilli(), status, now)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}
