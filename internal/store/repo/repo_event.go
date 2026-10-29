package repo

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type SQLiteEventRepository struct {
	db *sql.DB
}

func NewSQLiteEventRepository(db *sql.DB) *SQLiteEventRepository {
	return &SQLiteEventRepository{db: db}
}

func (r *SQLiteEventRepository) ListEventsSince(ctx context.Context, offset int64) ([]repo.EventRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT offset, topic, type, payload
		FROM events
		WHERE offset > ? ORDER BY offset ASC
	`, offset)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteEventRepository.ListEventsSince", err)
	}
	defer rows.Close()

	var events []repo.EventRow
	for rows.Next() {
		var ev repo.EventRow
		if err := rows.Scan(&ev.Offset, &ev.Topic, &ev.Type, &ev.Payload); err == nil {
			events = append(events, ev)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rows iteration error", err)
	}
	return events, nil
}
