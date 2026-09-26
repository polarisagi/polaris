package repo

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// apps 表 + plugins 表操作（R7 拆分自 repo_extension.go）。
// 结构体/构造函数/extension_instances/extension_catalog 见 repo_extension.go；
// mcp_servers/卸载清理见 repo_extension_mcp.go。
// ============================================================================

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

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SetPluginComponentsEnabled: commit", err)
	}
	return nil
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
