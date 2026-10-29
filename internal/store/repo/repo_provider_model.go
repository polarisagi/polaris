package repo

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// UpsertModel 插入或更新 provider_model。
func (r *SQLiteProviderRepository) UpsertModel(ctx context.Context, row types.ProviderModelRow) error {
	enabled := 0
	if row.Enabled {
		enabled = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO provider_models(id, provider_id, model_id, name, role, enabled, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  model_id=excluded.model_id, name=excluded.name, role=excluded.role,
		  enabled=excluded.enabled, updated_at=excluded.updated_at`,
		row.ID, row.ProviderID, row.ModelID, row.Name, row.Role, enabled, row.CreatedAt, row.UpdatedAt)
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
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ListModels rows", err)
	}
	return result, nil
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

// ActiveModelRoles 口径与 HandleGetModelRoles 一致：禁用模型/厂商持有的角色视为空缺。
func (r *SQLiteProviderRepository) ActiveModelRoles(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT m.role FROM provider_models m JOIN providers p ON p.id=m.provider_id
		  WHERE m.enabled=1 AND p.enabled=1`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ActiveModelRoles", err)
	}
	defer rows.Close()
	held := make(map[string]bool)
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ActiveModelRoles scan", err)
		}
		held[role] = true
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.ActiveModelRoles rows", err)
	}
	return held, nil
}

// CreateProviderWithModels 厂商与模型同事务落盘：此前分步写入，模型写失败被跳过后留下
// "厂商已建、角色缺失"的半套配置且接口仍返回成功。
func (r *SQLiteProviderRepository) CreateProviderWithModels(ctx context.Context, p types.ProviderRow, models []types.ProviderModelRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.CreateProviderWithModels begin", err)
	}
	defer tx.Rollback() //nolint:errcheck

	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	isLocal := 0
	if p.IsLocal {
		isLocal = 1
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO providers(id, name, type, base_url, api_key, project_id, location, sa_key_json, enabled, is_local, catalog_id, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Type, p.BaseURL, r.encrypt(p.APIKey), p.ProjectID, p.Location, p.SAKeyJSON,
		enabled, isLocal, p.CatalogID, p.CreatedAt, p.UpdatedAt); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.CreateProviderWithModels provider", err)
	}
	for _, m := range models {
		if m.Role == "default" || m.Role == "reasoning" {
			if _, err := tx.ExecContext(ctx, `UPDATE provider_models SET role='general' WHERE role=?`, m.Role); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.CreateProviderWithModels clear role", err)
			}
		}
		mEnabled := 0
		if m.Enabled {
			mEnabled = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO provider_models(id, provider_id, model_id, name, role, enabled, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?)`,
			m.ID, p.ID, m.ModelID, m.Name, m.Role, mEnabled, m.CreatedAt, m.UpdatedAt); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.CreateProviderWithModels model", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteProviderRepository.CreateProviderWithModels commit", err)
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
