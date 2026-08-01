package repo

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteTaskCheckpointRepository 实现 protocol.TaskCheckpointRepository。
type SQLiteTaskCheckpointRepository struct {
	db protocol.BlackboardDB
}

var _ protocol.TaskCheckpointRepository = (*SQLiteTaskCheckpointRepository)(nil)

func NewSQLiteTaskCheckpointRepository(db protocol.BlackboardDB) *SQLiteTaskCheckpointRepository {
	return &SQLiteTaskCheckpointRepository{db: db}
}

func (r *SQLiteTaskCheckpointRepository) GetCheckpoint(ctx context.Context, taskID, nodeID string, attempt int) (*types.TaskCheckpointRow, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT status, output_json, idempotency_key, taint_level, started_at, completed_at, error, resume_ctx_json
		 FROM task_checkpoints WHERE task_id = ? AND node_id = ? AND attempt = ?`,
		taskID, nodeID, attempt,
	)
	var cp types.TaskCheckpointRow
	var outputJSON, idempotencyKey, errStr, resumeCtxJSON sql.NullString
	var startedAt, completedAt sql.NullInt64

	err := row.Scan(&cp.Status, &outputJSON, &idempotencyKey, &cp.TaintLevel, &startedAt, &completedAt, &errStr, &resumeCtxJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskCheckpointRepository.GetCheckpoint", err)
	}

	cp.TaskID = taskID
	cp.NodeID = nodeID
	cp.Attempt = attempt
	cp.OutputJSON = outputJSON.String
	cp.IdempotencyKey = idempotencyKey.String
	cp.StartedAt = startedAt.Int64
	cp.CompletedAt = completedAt.Int64
	cp.Error = errStr.String
	cp.ResumeCtxJSON = resumeCtxJSON.String

	return &cp, nil
}

func (r *SQLiteTaskCheckpointRepository) GetLatestCheckpoint(ctx context.Context, taskID, nodeID string) (*types.TaskCheckpointRow, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT attempt, status, output_json, idempotency_key, taint_level, started_at, completed_at, error, resume_ctx_json
		 FROM task_checkpoints WHERE task_id = ? AND node_id = ? ORDER BY attempt DESC LIMIT 1`,
		taskID, nodeID,
	)
	var cp types.TaskCheckpointRow
	var outputJSON, idempotencyKey, errStr, resumeCtxJSON sql.NullString
	var startedAt, completedAt sql.NullInt64

	err := row.Scan(&cp.Attempt, &cp.Status, &outputJSON, &idempotencyKey, &cp.TaintLevel, &startedAt, &completedAt, &errStr, &resumeCtxJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskCheckpointRepository.GetLatestCheckpoint", err)
	}

	cp.TaskID = taskID
	cp.NodeID = nodeID
	cp.OutputJSON = outputJSON.String
	cp.IdempotencyKey = idempotencyKey.String
	cp.StartedAt = startedAt.Int64
	cp.CompletedAt = completedAt.Int64
	cp.Error = errStr.String
	cp.ResumeCtxJSON = resumeCtxJSON.String

	return &cp, nil
}

func (r *SQLiteTaskCheckpointRepository) UpsertCheckpoint(ctx context.Context, row types.TaskCheckpointRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO task_checkpoints (task_id, node_id, attempt, status, output_json, idempotency_key, taint_level, started_at, completed_at, error, resume_ctx_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id, node_id, attempt) DO UPDATE SET
		 status = excluded.status,
		 output_json = excluded.output_json,
		 idempotency_key = excluded.idempotency_key,
		 taint_level = excluded.taint_level,
		 started_at = excluded.started_at,
		 completed_at = excluded.completed_at,
		 error = excluded.error,
		 resume_ctx_json = excluded.resume_ctx_json`,
		row.TaskID, row.NodeID, row.Attempt, row.Status,
		sql.NullString{String: row.OutputJSON, Valid: row.OutputJSON != ""},
		sql.NullString{String: row.IdempotencyKey, Valid: row.IdempotencyKey != ""},
		row.TaintLevel,
		sql.NullInt64{Int64: row.StartedAt, Valid: row.StartedAt != 0},
		sql.NullInt64{Int64: row.CompletedAt, Valid: row.CompletedAt != 0},
		sql.NullString{String: row.Error, Valid: row.Error != ""},
		sql.NullString{String: row.ResumeCtxJSON, Valid: row.ResumeCtxJSON != ""},
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteTaskCheckpointRepository.UpsertCheckpoint", err)
	}
	return nil
}

func (r *SQLiteTaskCheckpointRepository) ListCheckpointsByTask(ctx context.Context, taskID string) ([]types.TaskCheckpointRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT node_id, attempt, status, output_json, idempotency_key, taint_level, started_at, completed_at, error
		 FROM task_checkpoints WHERE task_id = ? ORDER BY node_id ASC, attempt ASC`,
		taskID,
	)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskCheckpointRepository.ListCheckpointsByTask", err)
	}
	defer rows.Close()

	var res []types.TaskCheckpointRow
	for rows.Next() {
		var cp types.TaskCheckpointRow
		cp.TaskID = taskID
		var outputJSON, idempotencyKey, errStr sql.NullString
		var startedAt, completedAt sql.NullInt64

		if err := rows.Scan(&cp.NodeID, &cp.Attempt, &cp.Status, &outputJSON, &idempotencyKey, &cp.TaintLevel, &startedAt, &completedAt, &errStr); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskCheckpointRepository.ListCheckpointsByTask.Scan", err)
		}
		cp.OutputJSON = outputJSON.String
		cp.IdempotencyKey = idempotencyKey.String
		cp.StartedAt = startedAt.Int64
		cp.CompletedAt = completedAt.Int64
		cp.Error = errStr.String
		// Reason/ResumeCtxJSON not populated in this query historically（StateGraph
		// 消费方不需要）；两者已在 ListByStatus 中选取（AwaitingHandoffReconciler 消费方需要）。
		res = append(res, cp)
	}
	return res, nil
}

func (r *SQLiteTaskCheckpointRepository) ListByStatus(ctx context.Context, status string) ([]types.TaskCheckpointRow, error) {
	// 增加 ListByStatus 实现
	rows, err := r.db.QueryContext(ctx,
		`SELECT task_id, node_id, attempt, status, output_json, idempotency_key, taint_level, started_at, completed_at, error, reason, resume_ctx_json
		 FROM task_checkpoints WHERE status = ? ORDER BY started_at ASC`,
		status,
	)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskCheckpointRepository.ListByStatus", err)
	}
	defer rows.Close()

	var res []types.TaskCheckpointRow
	for rows.Next() {
		var cp types.TaskCheckpointRow
		var outputJSON, idempotencyKey, errStr, reasonStr, resumeCtxJSON sql.NullString
		var startedAt, completedAt sql.NullInt64

		if err := rows.Scan(&cp.TaskID, &cp.NodeID, &cp.Attempt, &cp.Status, &outputJSON, &idempotencyKey, &cp.TaintLevel, &startedAt, &completedAt, &errStr, &reasonStr, &resumeCtxJSON); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTaskCheckpointRepository.ListByStatus.Scan", err)
		}
		cp.OutputJSON = outputJSON.String
		cp.IdempotencyKey = idempotencyKey.String
		cp.StartedAt = startedAt.Int64
		cp.CompletedAt = completedAt.Int64
		cp.Error = errStr.String
		cp.Reason = reasonStr.String
		cp.ResumeCtxJSON = resumeCtxJSON.String
		res = append(res, cp)
	}
	return res, nil
}
