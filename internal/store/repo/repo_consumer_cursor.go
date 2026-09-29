package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteConsumerCursorRepository 读写统一游标表 consumer_cursors（002_outbox.sql，ADR-0104 决策九）。
// 消费端接口由各使用方（如 swarm/agents.WhisperCursorStore）在自己包内声明。
type SQLiteConsumerCursorRepository struct {
	db protocol.SQLQuerier
}

func NewSQLiteConsumerCursorRepository(db protocol.SQLQuerier) *SQLiteConsumerCursorRepository {
	return &SQLiteConsumerCursorRepository{db: db}
}

// GetCursor 返回消费者高水位；无记录返回 0（从头消费）。
func (r *SQLiteConsumerCursorRepository) GetCursor(ctx context.Context, consumerID string) (int64, error) {
	var seq int64
	err := r.db.QueryRowContext(ctx, "SELECT last_seq FROM consumer_cursors WHERE consumer_id = ?", consumerID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "ConsumerCursor.GetCursor", err)
	}
	return seq, nil
}

// SaveCursor 单调推进游标：新值不大于旧值时不回退，防止乱序写把高水位拉低而重推旧事件。
func (r *SQLiteConsumerCursorRepository) SaveCursor(ctx context.Context, consumerID string, seq int64) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO consumer_cursors (consumer_id, last_seq, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(consumer_id) DO UPDATE SET last_seq = excluded.last_seq, updated_at = excluded.updated_at
		 WHERE excluded.last_seq > consumer_cursors.last_seq`,
		consumerID, seq, time.Now().UnixMilli())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ConsumerCursor.SaveCursor", err)
	}
	return nil
}
