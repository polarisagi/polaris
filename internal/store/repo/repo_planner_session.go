package repo

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLitePlannerSessionRepository planner_sessions（031）的写入实现，供 planner.SessionRecorder 消费。
type SQLitePlannerSessionRepository struct {
	db protocol.SQLQuerier
}

func NewSQLitePlannerSessionRepository(db protocol.SQLQuerier) *SQLitePlannerSessionRepository {
	return &SQLitePlannerSessionRepository{db: db}
}

func (r *SQLitePlannerSessionRepository) Start(ctx context.Context, id, taskID, goal, taskType string, workerCount int) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO planner_sessions(id, task_id, goal, task_type, worker_count, status, created_at)
		 VALUES(?, ?, ?, ?, ?, 'running', ?)`,
		id, taskID, goal, taskType, workerCount, time.Now().UnixMilli())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLitePlannerSessionRepository.Start", err)
	}
	return nil
}

// Finish status ∈ {done, failed}。
func (r *SQLitePlannerSessionRepository) Finish(ctx context.Context, id, status string, score float64, engine string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE planner_sessions SET status=?, winning_score=?, winning_engine=?, completed_at=? WHERE id=?`,
		status, score, engine, time.Now().UnixMilli(), id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLitePlannerSessionRepository.Finish", err)
	}
	return nil
}
