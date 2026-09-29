package repo

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteTodoRepository session_todos（043）的读写实现。
type SQLiteTodoRepository struct {
	db protocol.BlackboardDB
}

var _ repo.TodoRepository = (*SQLiteTodoRepository)(nil)

func NewSQLiteTodoRepository(db protocol.BlackboardDB) *SQLiteTodoRepository {
	return &SQLiteTodoRepository{db: db}
}

func (r *SQLiteTodoRepository) ReplaceTodos(ctx context.Context, sessionID string, items []repo.TodoItem) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.ReplaceTodos begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_todos WHERE session_id=?`, sessionID); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.ReplaceTodos delete", err)
	}
	now := time.Now().UnixMilli()
	for i, it := range items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_todos(session_id, seq, content, status, updated_at) VALUES(?,?,?,?,?)`,
			sessionID, i, it.Content, it.Status, now); err != nil {
			return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.ReplaceTodos insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.ReplaceTodos commit", err)
	}
	return nil
}

func (r *SQLiteTodoRepository) ListTodos(ctx context.Context, sessionID string) ([]repo.TodoItem, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT content, status FROM session_todos WHERE session_id=? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.ListTodos", err)
	}
	defer rows.Close()
	out := []repo.TodoItem{}
	for rows.Next() {
		var it repo.TodoItem
		if err := rows.Scan(&it.Content, &it.Status); err != nil {
			return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.ListTodos scan", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.ListTodos rows", err)
	}
	return out, nil
}

func (r *SQLiteTodoRepository) DeleteTodos(ctx context.Context, sessionID string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM session_todos WHERE session_id=?`, sessionID); err != nil {
		return apperr.Wrap(apperr.CodeStorageUnavailable, "SQLiteTodoRepository.DeleteTodos", err)
	}
	return nil
}
