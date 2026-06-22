package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLiteExtensionRepository 实现 protocol.ExtensionRepository。
// 操作 extension_instances, extension_catalog, mcp_servers 表。
// @arch: docs/upgrade/repo-interface-migration.md §3.4
type SQLiteExtensionRepository struct {
	db *sql.DB
}

var _ protocol.ExtensionRepository = (*SQLiteExtensionRepository)(nil)

func NewSQLiteExtensionRepository(db *sql.DB) *SQLiteExtensionRepository {
	return &SQLiteExtensionRepository{db: db}
}

// --- extension_instances ---

func (r *SQLiteExtensionRepository) UpsertInstance(ctx context.Context, row types.ExtInstanceRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO extension_instances(id, ext_type, origin, catalog_id, name, publisher, trust_tier, runtime_id, install_path, config, status, error_msg, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  status=excluded.status, error_msg=excluded.error_msg, updated_at=excluded.updated_at,
		  install_path=excluded.install_path, config=excluded.config, name=excluded.name, trust_tier=excluded.trust_tier`,
		row.ID, row.ExtType, row.Origin, row.CatalogID, row.Name, row.Publisher, row.TrustTier, row.RuntimeID, row.InstallPath, row.Config, row.Status, row.ErrorMsg, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpsertInstance", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) GetInstance(ctx context.Context, id string) (*types.ExtInstanceRow, error) {
	var row types.ExtInstanceRow
	err := r.db.QueryRowContext(ctx,
		`SELECT id, ext_type, origin, catalog_id, name, publisher, trust_tier, runtime_id, install_path, config, status, error_msg, created_at, updated_at
		FROM extension_instances WHERE id=?`, id).Scan(
		&row.ID, &row.ExtType, &row.Origin, &row.CatalogID, &row.Name, &row.Publisher, &row.TrustTier, &row.RuntimeID, &row.InstallPath, &row.Config, &row.Status, &row.ErrorMsg, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetInstance", err)
	}
	return &row, nil
}

func (r *SQLiteExtensionRepository) UpdateInstanceStatus(ctx context.Context, id, status, errorMsg string) error {
	var errSql sql.NullString
	if errorMsg != "" {
		errSql = sql.NullString{String: errorMsg, Valid: true}
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE extension_instances SET status=?, error_msg=?, updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		status, errSql, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpdateInstanceStatus", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) UpdateInstanceInstallPath(ctx context.Context, id, installPath string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE extension_instances SET install_path=?, updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		installPath, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpdateInstanceInstallPath", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) ListInstances(ctx context.Context) ([]types.ExtInstanceRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, ext_type, origin, catalog_id, name, publisher, trust_tier, runtime_id, install_path, config, status, error_msg, created_at, updated_at
		FROM extension_instances ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListInstances", err)
	}
	defer rows.Close()

	var result []types.ExtInstanceRow
	for rows.Next() {
		var row types.ExtInstanceRow
		if err := rows.Scan(&row.ID, &row.ExtType, &row.Origin, &row.CatalogID, &row.Name, &row.Publisher, &row.TrustTier, &row.RuntimeID, &row.InstallPath, &row.Config, &row.Status, &row.ErrorMsg, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListInstances scan", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *SQLiteExtensionRepository) DeleteInstance(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM extension_instances WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteInstance", err)
	}
	return nil
}

// --- extension_catalog ---

func (r *SQLiteExtensionRepository) GetCatalogEntry(ctx context.Context, id string) (*types.ExtCatalogRow, error) {
	var row types.ExtCatalogRow
	err := r.db.QueryRowContext(ctx,
		`SELECT id, marketplace_id, type, name, description, publisher, trust_tier, url, payload, updated_at
		FROM extension_catalog WHERE id=?`, id).Scan(
		&row.ID, &row.MarketplaceID, &row.Type, &row.Name, &row.Description, &row.Publisher, &row.TrustTier, &row.URL, &row.Payload, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetCatalogEntry", err)
	}
	return &row, nil
}

func (r *SQLiteExtensionRepository) SearchCatalog(ctx context.Context, query string, limit int) ([]types.ExtCatalogRow, error) {
	pattern := "%" + query + "%"
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, marketplace_id, type, name, description, publisher, trust_tier, url, payload, updated_at
		FROM extension_catalog WHERE name LIKE ? OR description LIKE ? LIMIT ?`, pattern, pattern, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SearchCatalog", err)
	}
	defer rows.Close()

	var result []types.ExtCatalogRow
	for rows.Next() {
		var row types.ExtCatalogRow
		if err := rows.Scan(&row.ID, &row.MarketplaceID, &row.Type, &row.Name, &row.Description, &row.Publisher, &row.TrustTier, &row.URL, &row.Payload, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SearchCatalog scan", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *SQLiteExtensionRepository) ListCatalogByIDs(ctx context.Context, ids []string) ([]types.ExtCatalogRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT id, marketplace_id, type, name, description, publisher, trust_tier, url, payload, updated_at
		FROM extension_catalog WHERE id IN (%s)`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListCatalogByIDs", err)
	}
	defer rows.Close()

	var result []types.ExtCatalogRow
	for rows.Next() {
		var row types.ExtCatalogRow
		if err := rows.Scan(&row.ID, &row.MarketplaceID, &row.Type, &row.Name, &row.Description, &row.Publisher, &row.TrustTier, &row.URL, &row.Payload, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListCatalogByIDs scan", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *SQLiteExtensionRepository) ReplaceMarketplaceCatalog(ctx context.Context, marketplaceID string, entries []types.ExtCatalogRow) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, "DELETE FROM extension_catalog WHERE marketplace_id = ?", marketplaceID); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "db error", err)
	}

	syncedCount := 0
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO extension_catalog(id, marketplace_id, type, name, description, publisher, trust_tier, url, payload) 
			VALUES(?,?,?,?,?,?,?,?,?)`,
			e.ID, marketplaceID, e.Type, e.Name, e.Description, e.Publisher, e.TrustTier, e.URL, e.Payload); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
		syncedCount++
	}

	return syncedCount, tx.Commit()
}

func (r *SQLiteExtensionRepository) DeleteOrphanCatalogEntries(ctx context.Context, activeMarketplaceIDs []any) error {
	if len(activeMarketplaceIDs) > 0 {
		queryMarks := ""
		for i := range activeMarketplaceIDs {
			if i > 0 {
				queryMarks += ","
			}
			queryMarks += "?"
		}
		delOrphanQuery := "DELETE FROM extension_catalog WHERE marketplace_id != 'builtin' AND marketplace_id NOT IN (" + queryMarks + ")"
		_, err := r.db.ExecContext(ctx, delOrphanQuery, activeMarketplaceIDs...)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteOrphanCatalogEntries", err)
		}
		return nil
	}
	_, err := r.db.ExecContext(ctx, "DELETE FROM extension_catalog WHERE marketplace_id != 'builtin'")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) SeedMarketplace(ctx context.Context, row protocol.Marketplace) error {
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO plugin_marketplaces(id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, sort_order, created_at)
					VALUES(?,?,?,?,?,?,1,?,1,?,?)`,
		row.ID, row.Name, row.Type, row.Publisher, row.RepoURL, row.Description, row.TrustTier, row.SortOrder, row.CreatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) CreateMarketplace(ctx context.Context, row protocol.Marketplace) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO plugin_marketplaces
		 (id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, sort_order, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.Name, row.Type, row.Publisher, row.RepoURL,
		row.Description, row.IsBuiltin, row.TrustTier, row.Enabled, row.SortOrder, row.CreatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.CreateMarketplace", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) UpdateMarketplace(ctx context.Context, id string, row protocol.Marketplace) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE plugin_marketplaces SET name=?, type=?, publisher=?, repo_url=?, description=?, trust_tier=?, enabled=?, sort_order=? WHERE id=? AND is_builtin=0`,
		row.Name, row.Type, row.Publisher, row.RepoURL, row.Description, row.TrustTier, row.Enabled, row.SortOrder, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpdateMarketplace", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) UpdateMarketplaceSortOrder(ctx context.Context, id string, sortOrder int) error {
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_marketplaces SET sort_order=? WHERE id=?`, sortOrder, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpdateMarketplaceSortOrder", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) DeleteMarketplace(ctx context.Context, id string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM plugin_marketplaces WHERE id=? AND is_builtin=0`, id)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteMarketplace", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *SQLiteExtensionRepository) GetMaxMarketplaceSortOrder(ctx context.Context) (int, error) {
	var maxOrder int
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order), 90) FROM plugin_marketplaces`).Scan(&maxOrder)
	if err != nil {
		return 90, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetMaxMarketplaceSortOrder", err)
	}
	return maxOrder, nil
}

func (r *SQLiteExtensionRepository) SeedCatalogEntry(ctx context.Context, row types.ExtCatalogRow) error {
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO extension_catalog(id, marketplace_id, type, name, description, publisher, trust_tier, url, payload)
					VALUES(?,?,?,?,?,?,?,?,?)`,
		row.ID, row.MarketplaceID, row.Type, row.Name, row.Description, row.Publisher, row.TrustTier, row.URL, row.Payload)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

// --- mcp_servers ---

func (r *SQLiteExtensionRepository) ListMCPServers(ctx context.Context) ([]types.MCPServerRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, transport, command, args, env, url, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, created_at, updated_at
		FROM mcp_servers ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListMCPServers", err)
	}
	defer rows.Close()

	var result []types.MCPServerRow
	for rows.Next() {
		var row types.MCPServerRow
		var enabledInt int
		if err := rows.Scan(&row.ID, &row.Name, &row.Transport, &row.Command, &row.Args, &row.Env, &row.URL, &enabledInt, &row.Timeout, &row.TrustTier, &row.CatalogID, &row.PluginID, &row.WorkDir, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListMCPServers scan", err)
		}
		row.Enabled = enabledInt == 1
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *SQLiteExtensionRepository) GetMCPServer(ctx context.Context, id string) (*types.MCPServerRow, error) {
	var row types.MCPServerRow
	var enabledInt int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, transport, command, args, env, url, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, created_at, updated_at
		FROM mcp_servers WHERE id=?`, id).Scan(
		&row.ID, &row.Name, &row.Transport, &row.Command, &row.Args, &row.Env, &row.URL, &enabledInt, &row.Timeout, &row.TrustTier, &row.CatalogID, &row.PluginID, &row.WorkDir, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetMCPServer", err)
	}
	row.Enabled = enabledInt == 1
	return &row, nil
}

func (r *SQLiteExtensionRepository) UpsertMCPServer(ctx context.Context, row types.MCPServerRow) error {
	enabledInt := 0
	if row.Enabled {
		enabledInt = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO mcp_servers(id, name, transport, command, args, env, url, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, transport=excluded.transport, command=excluded.command,
		  args=excluded.args, env=excluded.env, url=excluded.url, enabled=excluded.enabled,
		  timeout=excluded.timeout, trust_tier=excluded.trust_tier, work_dir=excluded.work_dir, updated_at=excluded.updated_at`,
		row.ID, row.Name, row.Transport, row.Command, row.Args, row.Env, row.URL, enabledInt, row.Timeout, row.TrustTier, row.CatalogID, row.PluginID, row.WorkDir, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpsertMCPServer", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) UpdateMCPServer(ctx context.Context, id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	setClauses := make([]string, 0, len(fields))
	args := make([]any, 0, len(fields)+1)
	for k, v := range fields {
		setClauses = append(setClauses, fmt.Sprintf("%s=?", k))
		args = append(args, v)
	}
	args = append(args, id)
	query := fmt.Sprintf(`UPDATE mcp_servers SET %s WHERE id=?`, strings.Join(setClauses, ", "))
	_, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpdateMCPServer", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) DeleteMCPServer(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteMCPServer", err)
	}
	return nil
}

// --- Cleanup ---

func (r *SQLiteExtensionRepository) UninstallCleanup(ctx context.Context, id, runtimeID, extType string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup begin", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if extType == "mcp" {
		_, err = tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE plugin_id=? OR id=?`, id, runtimeID)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup mcp", err)
		}
	} else if extType == "native" || extType == "plugin" {
		_, err = tx.ExecContext(ctx, `DELETE FROM skills WHERE plugin_id=?`, id)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skills", err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM apps WHERE origin=?`, id)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup apps", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup commit", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) DeleteInstancesByPluginID(ctx context.Context, pluginID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM extension_instances WHERE id=?`, pluginID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteInstancesByPluginID", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) DeleteCatalogEntry(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM extension_catalog WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteCatalogEntry", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) IsCatalogBuiltin(ctx context.Context, id string) (bool, error) {
	var count int
	// NOTE: The marketplace_id is checked in manager.go to see if it's builtin.
	// The builtin plugin_marketplaces has is_builtin=1.
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM extension_catalog ec 
		JOIN plugin_marketplaces pm ON ec.marketplace_id = pm.id 
		WHERE ec.id=? AND pm.is_builtin=1`, id).Scan(&count)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.IsCatalogBuiltin", err)
	}
	return count > 0, nil
}

// --- apps ---

func (r *SQLiteExtensionRepository) UpsertApp(ctx context.Context, row types.AppRow) error {
	enabledInt := 0
	if row.Enabled {
		enabledInt = 1
	}
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO apps(id, name, display_name, description, url, publisher, enabled, trust_tier, catalog_id, created_at, updated_at) "+
			"VALUES(?,?,?,?,?,?,?,?,?,?,?) "+
			"ON CONFLICT(id) DO UPDATE SET "+
			"name=excluded.name, display_name=excluded.display_name, description=excluded.description, url=excluded.url, "+
			"publisher=excluded.publisher, enabled=excluded.enabled, trust_tier=excluded.trust_tier, "+
			"catalog_id=excluded.catalog_id, updated_at=excluded.updated_at",
		row.ID, row.Name, row.DisplayName, row.Description, row.URL, row.Publisher, enabledInt, row.TrustTier, row.CatalogID, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpsertApp", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) DeleteApp(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM apps WHERE id=?", id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "error", err)
	}
	return nil
}

// --- plugins ---

func (r *SQLiteExtensionRepository) UpdatePluginStatus(ctx context.Context, id string, enabled int, mcpPolicy string, now string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE plugins SET enabled=?, mcp_policy=?, updated_at=? WHERE id=?",
		enabled, mcpPolicy, now, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "error", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) SetPluginComponentsEnabled(ctx context.Context, pluginID string, enabled int, now string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SetPluginComponentsEnabled", err)
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx, "UPDATE mcp_servers SET enabled=?, updated_at=? WHERE plugin_id=?", enabled, now, pluginID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SetPluginComponentsEnabled", err)
	}

	deprecated := 0
	if enabled == 0 {
		deprecated = 1
	}
	_, err = tx.ExecContext(ctx, "UPDATE skills SET deprecated=?, updated_at=? WHERE plugin_id=?", deprecated, now, pluginID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SetPluginComponentsEnabled", err)
	}

	return tx.Commit()
}

func (r *SQLiteExtensionRepository) UpdatePluginMCPServerEnabled(ctx context.Context, pluginID, serverID string, enabled int, now string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE mcp_servers SET enabled=?, updated_at=? WHERE id=? AND plugin_id=?",
		enabled, now, serverID, pluginID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "error", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) UpsertPlugin(ctx context.Context, id, name, version, displayName, description, publisher, homepage, installPath string, enabled, trustTier int, catalogID, mcpPolicy, manifest, createdAt, updatedAt string) error {
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO plugins(id, name, version, display_name, description, publisher, homepage, install_path, enabled, trust_tier, catalog_id, mcp_policy, manifest, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name, version=excluded.version, display_name=excluded.display_name, description=excluded.description, publisher=excluded.publisher, homepage=excluded.homepage, install_path=excluded.install_path, enabled=excluded.enabled, trust_tier=excluded.trust_tier, catalog_id=excluded.catalog_id, mcp_policy=excluded.mcp_policy, manifest=excluded.manifest, updated_at=excluded.updated_at",
		id, name, version, displayName, description, publisher, homepage, installPath, enabled, trustTier, catalogID, mcpPolicy, manifest, createdAt, updatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "error", err)
	}
	return nil
}
