package repo

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"context"
	"database/sql"
	"fmt"

	"github.com/polarisagi/polaris/internal/protocol"
)

type SQLiteChannelRepository struct {
	db protocol.SQLQuerier
}

var _ repo.ChannelRepository = (*SQLiteChannelRepository)(nil)

func NewSQLiteChannelRepository(db protocol.SQLQuerier) *SQLiteChannelRepository {
	return &SQLiteChannelRepository{db: db}
}

func (r *SQLiteChannelRepository) CreateChannel(ctx context.Context, row repo.ChannelRow) error {
	enabledInt := 0
	if row.Enabled {
		enabledInt = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO channels(id,name,type,enabled,config_json,webhook_secret,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		row.ID, row.Name, row.Type, enabledInt, row.ConfigJSON, row.WebhookSecret, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return fmt.Errorf("SQLiteChannelRepository.CreateChannel: %w", err)
	}
	return nil
}

func (r *SQLiteChannelRepository) UpdateChannel(ctx context.Context, row repo.ChannelRow) (bool, error) {
	enabledInt := 0
	if row.Enabled {
		enabledInt = 1
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE channels SET name=?,type=?,enabled=?,config_json=?,updated_at=? WHERE id=?`,
		row.Name, row.Type, enabledInt, row.ConfigJSON, row.UpdatedAt, row.ID)
	if err != nil {
		return false, fmt.Errorf("SQLiteChannelRepository.UpdateChannel: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *SQLiteChannelRepository) DeleteChannel(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("SQLiteChannelRepository.DeleteChannel: %w", err)
	}
	return nil
}

func (r *SQLiteChannelRepository) ListChannels(ctx context.Context) ([]repo.ChannelRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name, type, enabled, config_json, webhook_secret, created_at, updated_at FROM channels ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("SQLiteChannelRepository.ListChannels: %w", err)
	}
	defer rows.Close()

	var result []repo.ChannelRow
	for rows.Next() {
		var row repo.ChannelRow
		var enabledInt int
		if err := rows.Scan(&row.ID, &row.Name, &row.Type, &enabledInt, &row.ConfigJSON, &row.WebhookSecret, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("SQLiteChannelRepository.ListChannels scan: %w", err)
		}
		row.Enabled = enabledInt == 1
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *SQLiteChannelRepository) GetChannel(ctx context.Context, id string) (*repo.ChannelRow, error) {
	var row repo.ChannelRow
	var enabledInt int
	err := r.db.QueryRowContext(ctx, `SELECT id, name, type, enabled, config_json, webhook_secret, created_at, updated_at FROM channels WHERE id=?`, id).
		Scan(&row.ID, &row.Name, &row.Type, &enabledInt, &row.ConfigJSON, &row.WebhookSecret, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("SQLiteChannelRepository.GetChannel: %w", err)
	}
	row.Enabled = enabledInt == 1
	return &row, nil
}
