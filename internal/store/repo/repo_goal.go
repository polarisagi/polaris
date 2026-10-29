package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const defaultGoalMaxRounds = 20

// SQLiteGoalRepository agent_goals（053）的读写实现。
type SQLiteGoalRepository struct {
	db protocol.BlackboardDB
}

var _ repo.GoalRepository = (*SQLiteGoalRepository)(nil)

func NewSQLiteGoalRepository(db protocol.BlackboardDB) *SQLiteGoalRepository {
	return &SQLiteGoalRepository{db: db}
}

const goalCols = `id, session_id, objective, done_criteria, status, note, max_rounds, rounds_used, idle_rounds`

func scanGoal(row interface{ Scan(...any) error }) (*repo.Goal, error) {
	var g repo.Goal
	if err := row.Scan(&g.ID, &g.SessionID, &g.Objective, &g.DoneCriteria, &g.Status, &g.Note,
		&g.MaxRounds, &g.RoundsUsed, &g.IdleRounds); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository scan", err)
	}
	return &g, nil
}

func (r *SQLiteGoalRepository) SetGoal(ctx context.Context, sessionID, objective, doneCriteria string, maxRounds int) (repo.Goal, error) {
	if maxRounds <= 0 {
		maxRounds = defaultGoalMaxRounds
	}
	now := time.Now().UnixMilli()
	g := repo.Goal{
		ID: "goal_" + uuid.NewString(), SessionID: sessionID, Objective: objective, DoneCriteria: doneCriteria,
		Status: repo.GoalActive, MaxRounds: maxRounds,
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return g, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.SetGoal begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_goals SET status='paused', updated_at=? WHERE session_id=? AND status='active'`, now, sessionID); err != nil {
		return g, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.SetGoal pause old", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_goals(`+goalCols+`, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.SessionID, g.Objective, g.DoneCriteria, g.Status, "", g.MaxRounds, 0, 0, now, now); err != nil {
		return g, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.SetGoal insert", err)
	}
	if err := tx.Commit(); err != nil {
		return g, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.SetGoal commit", err)
	}
	return g, nil
}

func (r *SQLiteGoalRepository) CurrentGoal(ctx context.Context, sessionID string) (*repo.Goal, error) {
	return scanGoal(r.db.QueryRowContext(ctx,
		`SELECT `+goalCols+` FROM agent_goals WHERE session_id=? ORDER BY updated_at DESC, created_at DESC LIMIT 1`, sessionID))
}

func (r *SQLiteGoalRepository) ActiveGoal(ctx context.Context, sessionID string) (*repo.Goal, error) {
	return scanGoal(r.db.QueryRowContext(ctx,
		`SELECT `+goalCols+` FROM agent_goals WHERE session_id=? AND status='active'`, sessionID))
}

func (r *SQLiteGoalRepository) UpdateGoal(ctx context.Context, sessionID, status, note string) error {
	g, err := r.CurrentGoal(ctx, sessionID)
	if err != nil {
		return err
	}
	if g == nil {
		return apperr.New(apperr.CodeNotFound, "goal: no goal in this session")
	}
	if status == "" {
		status = g.Status
	}
	if note == "" {
		note = g.Note
	}
	if status == repo.GoalActive && g.Status != repo.GoalActive {
		// 重新激活时清零空转计数；唯一 active 索引兜底并发。
		if _, err := r.db.ExecContext(ctx,
			`UPDATE agent_goals SET status=?, note=?, idle_rounds=0, updated_at=? WHERE id=?`,
			status, note, time.Now().UnixMilli(), g.ID); err != nil {
			return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.UpdateGoal", err)
		}
		return nil
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE agent_goals SET status=?, note=?, updated_at=? WHERE id=?`,
		status, note, time.Now().UnixMilli(), g.ID); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.UpdateGoal", err)
	}
	return nil
}

func (r *SQLiteGoalRepository) BeginRound(ctx context.Context, goalID string) (*repo.Goal, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE agent_goals SET rounds_used=rounds_used+1, updated_at=? WHERE id=? AND status='active'`,
		time.Now().UnixMilli(), goalID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.BeginRound", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	return r.byID(ctx, goalID)
}

func (r *SQLiteGoalRepository) FinishRound(ctx context.Context, goalID string, idle bool, maxIdle int) (*repo.Goal, error) {
	now := time.Now().UnixMilli()
	var err error
	if idle {
		// 仍为 active 才累加；达到 maxIdle 自动 paused。
		_, err = r.db.ExecContext(ctx, `UPDATE agent_goals SET idle_rounds=idle_rounds+1,
			status=CASE WHEN status='active' AND idle_rounds+1>=? THEN 'paused' ELSE status END, updated_at=?
			WHERE id=? AND status='active'`, maxIdle, now, goalID)
	} else {
		_, err = r.db.ExecContext(ctx, `UPDATE agent_goals SET idle_rounds=0, updated_at=? WHERE id=?`, now, goalID)
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.FinishRound", err)
	}
	return r.byID(ctx, goalID)
}

func (r *SQLiteGoalRepository) PauseGoal(ctx context.Context, sessionID string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE agent_goals SET status='paused', updated_at=? WHERE session_id=? AND status='active'`,
		time.Now().UnixMilli(), sessionID); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteGoalRepository.PauseGoal", err)
	}
	return nil
}

func (r *SQLiteGoalRepository) byID(ctx context.Context, id string) (*repo.Goal, error) {
	return scanGoal(r.db.QueryRowContext(ctx, `SELECT `+goalCols+` FROM agent_goals WHERE id=?`, id))
}
