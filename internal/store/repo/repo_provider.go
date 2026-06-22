package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteProviderRepository 实现 protocol.ProviderRepository。
// 操作 providers + provider_models 表。
// @arch: docs/upgrade/repo-interface-migration.md §3.2
type SQLiteProviderRepository struct {
	db *sql.DB
}

var _ protocol.ProviderRepository = (*SQLiteProviderRepository)(nil)

// NewSQLiteProviderRepository 创建 SQLiteProviderRepository。
func NewSQLiteProviderRepository(db *sql.DB) *SQLiteProviderRepository {
	return &SQLiteProviderRepository{db: db}
}

// UpsertProvider 插入或更新 provider 记录。
func (r *SQLiteProviderRepository) UpsertProvider(ctx context.Context, row types.ProviderRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO providers(id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, catalog_id, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, type=excluded.type, base_url=excluded.base_url,
		  api_key=excluded.api_key, project_id=excluded.project_id, location=excluded.location,
		  sa_key_json=excluded.sa_key_json, enabled=excluded.enabled,
		  catalog_id=excluded.catalog_id, updated_at=excluded.updated_at`,
		row.ID, row.Name, row.Type, row.BaseURL, row.APIKey, row.ProjectID, row.Location,
		row.SAKeyJSON, row.Enabled, row.CatalogID, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.UpsertProvider", err)
	}
	return nil
}

// GetProvider 按 ID 获取 provider。
func (r *SQLiteProviderRepository) GetProvider(ctx context.Context, id string) (*types.ProviderRow, error) {
	row := &types.ProviderRow{}
	var enabledInt int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, catalog_id, created_at, updated_at
		FROM providers WHERE id=?`, id,
	).Scan(&row.ID, &row.Name, &row.Type, &row.BaseURL, &row.APIKey, &row.ProjectID, &row.Location,
		&row.SAKeyJSON, &enabledInt, &row.CatalogID, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.GetProvider", err)
	}
	row.Enabled = enabledInt == 1
	return row, nil
}

// ListProviders 返回所有 provider。
func (r *SQLiteProviderRepository) ListProviders(ctx context.Context) ([]types.ProviderRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, catalog_id, created_at, updated_at
		FROM providers ORDER BY created_at`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ListProviders", err)
	}
	defer rows.Close()
	var result []types.ProviderRow
	for rows.Next() {
		var row types.ProviderRow
		var enabledInt int
		if err := rows.Scan(&row.ID, &row.Name, &row.Type, &row.BaseURL, &row.APIKey, &row.ProjectID,
			&row.Location, &row.SAKeyJSON, &enabledInt, &row.CatalogID, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ListProviders scan", err)
		}
		row.Enabled = enabledInt == 1
		result = append(result, row)
	}
	return result, rows.Err()
}

// DeleteProvider 删除 provider。
func (r *SQLiteProviderRepository) DeleteProvider(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.DeleteProvider", err)
	}
	return nil
}

// UpsertModel 插入或更新 provider_model。
func (r *SQLiteProviderRepository) UpsertModel(ctx context.Context, row types.ProviderModelRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO provider_models(id, provider_id, model_id, name, role, enabled, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  model_id=excluded.model_id, name=excluded.name, role=excluded.role,
		  enabled=excluded.enabled, updated_at=excluded.updated_at`,
		row.ID, row.ProviderID, row.ModelID, row.Name, row.Role, row.Enabled, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.UpsertModel", err)
	}
	return nil
}

// ListModels 返回指定 provider 的所有 model。
func (r *SQLiteProviderRepository) ListModels(ctx context.Context, providerID string) ([]types.ProviderModelRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, provider_id, model_id, name, role, enabled, created_at, updated_at
		FROM provider_models WHERE provider_id=? ORDER BY created_at`, providerID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ListModels", err)
	}
	defer rows.Close()
	var result []types.ProviderModelRow
	for rows.Next() {
		var row types.ProviderModelRow
		var enabledInt int
		if err := rows.Scan(&row.ID, &row.ProviderID, &row.ModelID, &row.Name, &row.Role, &enabledInt, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ListModels scan", err)
		}
		row.Enabled = enabledInt == 1
		result = append(result, row)
	}
	return result, rows.Err()
}

// DeleteModelsByProvider 删除指定 provider 的所有 model。
func (r *SQLiteProviderRepository) DeleteModelsByProvider(ctx context.Context, providerID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM provider_models WHERE provider_id=?`, providerID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.DeleteModelsByProvider", err)
	}
	return nil
}

func (r *SQLiteProviderRepository) ClearModelRoles(ctx context.Context, targetRoles []string, exceptID string) error {
	if len(targetRoles) == 0 {
		return nil
	}
	marks := ""
	args := make([]any, 0, len(targetRoles)+1)
	for i, tr := range targetRoles {
		if i > 0 {
			marks += ","
		}
		marks += "?"
		args = append(args, tr)
	}

	query := `UPDATE provider_models SET role='general' WHERE role IN (` + marks + `)`
	if exceptID != "" {
		query += ` AND id != ?`
		args = append(args, exceptID)
	}
	_, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteProviderRepository) SetModelRole(ctx context.Context, id string, role string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE provider_models SET role=? WHERE id=?`, role, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

// SeedIfEmpty 仅在 providers 表为空时插入默认配置；幂等。操作。
func (r *SQLiteProviderRepository) SeedIfEmpty(ctx context.Context, rows []types.ProviderRow, models []types.ProviderModelRow) error {
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM providers`).Scan(&count); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.SeedIfEmpty count", err)
	}
	if count > 0 {
		return nil // 已有数据，不操作
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.SeedIfEmpty begin", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, row := range rows {
		if row.CreatedAt == "" {
			row.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if row.UpdatedAt == "" {
			row.UpdatedAt = row.CreatedAt
		}
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO providers(id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, catalog_id, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.ID, row.Name, row.Type, row.BaseURL, row.APIKey, row.ProjectID, row.Location,
			row.SAKeyJSON, row.Enabled, row.CatalogID, row.CreatedAt, row.UpdatedAt)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.SeedIfEmpty insert provider", err)
		}
	}
	for _, model := range models {
		if model.CreatedAt == "" {
			model.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if model.UpdatedAt == "" {
			model.UpdatedAt = model.CreatedAt
		}
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO provider_models(id, provider_id, model_id, name, role, enabled, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			model.ID, model.ProviderID, model.ModelID, model.Name, model.Role, model.Enabled, model.CreatedAt, model.UpdatedAt)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.SeedIfEmpty insert model", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.SeedIfEmpty commit", err)
	}
	return nil
}

func (r *SQLiteProviderRepository) SeedFromEnv(ctx context.Context, p types.ProviderRow) (bool, error) {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	res, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO providers
			(id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, '', '', '', ?, ?, ?)`,
		p.ID, p.Name, p.Type, p.BaseURL, p.APIKey, enabled, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	inserted, _ := res.RowsAffected()
	return inserted > 0, nil
}

func (r *SQLiteProviderRepository) UpdateProviderAPIKey(ctx context.Context, id, apiKey, updatedAt string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE providers SET api_key=?, updated_at=? WHERE id=?`,
		apiKey, updatedAt, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteProviderRepository) SeedModelFromEnv(ctx context.Context, m types.ProviderModelRow) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO provider_models
			(id, provider_id, model_id, name, role, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ProviderID, m.ModelID, m.Name, m.Role, enabled, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteProviderRepository) CreateProvider(ctx context.Context, p types.ProviderRow) error {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO providers(id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, catalog_id, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Type, p.BaseURL, p.APIKey, p.ProjectID, p.Location, p.SAKeyJSON, enabled, p.CatalogID, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteProviderRepository) UpdateProvider(ctx context.Context, id string, p types.ProviderRow) error {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE providers SET
		 name=?, type=?, base_url=?, api_key=?, project_id=?, location=?, sa_key_json=?, enabled=?, catalog_id=?, updated_at=?
		 WHERE id=?`,
		p.Name, p.Type, p.BaseURL, p.APIKey, p.ProjectID, p.Location, p.SAKeyJSON, enabled, p.CatalogID, p.UpdatedAt, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteProviderRepository) DeleteModel(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM provider_models WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}
