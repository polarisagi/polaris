package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

type SQLiteAppRepository struct {
	db *sql.DB
}

// NewSQLiteAppRepository 创建一个基于 SQLite 的 App 仓库实例
func NewSQLiteAppRepository(db *sql.DB) *SQLiteAppRepository {
	return &SQLiteAppRepository{db: db}
}

func (r *SQLiteAppRepository) CreateApp(ctx context.Context, app *protocol.App) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if app.CreatedAt == "" {
		app.CreatedAt = now
	}
	app.UpdatedAt = now

	enabled := 0
	if app.Enabled {
		enabled = 1
	}

	query := `
		INSERT INTO apps (
			id, name, display_name, description, url, publisher, enabled, trust_tier, catalog_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := r.db.ExecContext(ctx, query,
		app.ID, app.Name, app.DisplayName, app.Description, app.URL, app.Publisher,
		enabled, app.TrustTier, app.CatalogID, app.CreatedAt, app.UpdatedAt,
	)
	return err
}

func (r *SQLiteAppRepository) GetApp(ctx context.Context, id string) (*protocol.App, error) {
	query := `
		SELECT id, name, display_name, description, url, publisher, enabled, trust_tier, catalog_id, created_at, updated_at
		FROM apps
		WHERE id = ?
	`
	row := r.db.QueryRowContext(ctx, query, id)
	return r.scanApp(row)
}

func (r *SQLiteAppRepository) ListApps(ctx context.Context, enabledOnly bool) ([]*protocol.App, error) {
	query := `
		SELECT id, name, display_name, description, url, publisher, enabled, trust_tier, catalog_id, created_at, updated_at
		FROM apps
	`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*protocol.App
	for rows.Next() {
		app, err := r.scanAppRow(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return apps, nil
}

func (r *SQLiteAppRepository) UpdateApp(ctx context.Context, app *protocol.App) error {
	now := time.Now().UTC().Format(time.RFC3339)
	app.UpdatedAt = now

	enabled := 0
	if app.Enabled {
		enabled = 1
	}

	query := `
		UPDATE apps
		SET name = ?, display_name = ?, description = ?, url = ?, publisher = ?, enabled = ?, trust_tier = ?, catalog_id = ?, updated_at = ?
		WHERE id = ?
	`
	_, err := r.db.ExecContext(ctx, query,
		app.Name, app.DisplayName, app.Description, app.URL, app.Publisher,
		enabled, app.TrustTier, app.CatalogID, app.UpdatedAt, app.ID,
	)
	return err
}

func (r *SQLiteAppRepository) DeleteApp(ctx context.Context, id string) error {
	query := `DELETE FROM apps WHERE id = ?`
	_, err := r.db.ExecContext(ctx, query, id)
	return err
}

func (r *SQLiteAppRepository) SetAppEnabled(ctx context.Context, id string, enabled bool) error {
	now := time.Now().UTC().Format(time.RFC3339)
	val := 0
	if enabled {
		val = 1
	}
	query := `UPDATE apps SET enabled = ?, updated_at = ? WHERE id = ?`
	_, err := r.db.ExecContext(ctx, query, val, now, id)
	return err
}

func (r *SQLiteAppRepository) scanApp(row *sql.Row) (*protocol.App, error) {
	app := &protocol.App{}
	var enabled int
	err := row.Scan(
		&app.ID, &app.Name, &app.DisplayName, &app.Description, &app.URL, &app.Publisher,
		&enabled, &app.TrustTier, &app.CatalogID, &app.CreatedAt, &app.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	app.Enabled = enabled == 1
	return app, nil
}

func (r *SQLiteAppRepository) scanAppRow(row *sql.Rows) (*protocol.App, error) {
	app := &protocol.App{}
	var enabled int
	err := row.Scan(
		&app.ID, &app.Name, &app.DisplayName, &app.Description, &app.URL, &app.Publisher,
		&enabled, &app.TrustTier, &app.CatalogID, &app.CreatedAt, &app.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	app.Enabled = enabled == 1
	return app, nil
}
