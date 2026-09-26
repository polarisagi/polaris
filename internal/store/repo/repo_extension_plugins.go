package repo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// plugins 表操作（R7 拆分自 repo_extension.go）。
// 结构体/构造函数/extension_instances/extension_catalog 见 repo_extension.go；
// mcp_servers/卸载清理见 repo_extension_mcp.go。
// ============================================================================

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

func (r *SQLiteExtensionRepository) UpsertPlugin(ctx context.Context, row types.PluginRow) error {
	enabled := 0
	if row.Enabled {
		enabled = 1
	}
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO plugins(id, name, version, display_name, description, publisher, homepage, install_path, enabled, trust_tier, catalog_id, mcp_policy, manifest, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name, version=excluded.version, display_name=excluded.display_name, description=excluded.description, publisher=excluded.publisher, homepage=excluded.homepage, install_path=excluded.install_path, enabled=excluded.enabled, trust_tier=excluded.trust_tier, catalog_id=excluded.catalog_id, mcp_policy=excluded.mcp_policy, manifest=excluded.manifest, updated_at=excluded.updated_at",
		row.ID, row.Name, row.Version, row.DisplayName, row.Description, row.Publisher, row.Homepage, row.InstallPath,
		enabled, row.TrustTier, row.CatalogID, row.MCPPolicy, row.Manifest, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpsertPlugin", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) GetPluginInstallPath(ctx context.Context, pluginID string) (string, error) {
	var path string
	err := r.db.QueryRowContext(ctx, "SELECT install_path FROM plugins WHERE id=?", pluginID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", apperr.New(apperr.CodeNotFound, "plugin not found: "+pluginID)
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetPluginInstallPath", err)
	}
	return path, nil
}

func (r *SQLiteExtensionRepository) GetPluginManifest(ctx context.Context, pluginID string) (string, error) {
	var manifest string
	err := r.db.QueryRowContext(ctx, "SELECT manifest FROM plugins WHERE id=?", pluginID).Scan(&manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", apperr.New(apperr.CodeNotFound, "plugin not found: "+pluginID)
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetPluginManifest", err)
	}
	return manifest, nil
}

func (r *SQLiteExtensionRepository) DeletePluginComponents(ctx context.Context, pluginID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeletePluginComponents", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{`DELETE FROM mcp_servers WHERE plugin_id=?`, `DELETE FROM skills WHERE plugin_id=?`} {
		if _, err := tx.ExecContext(ctx, q, pluginID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeletePluginComponents", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeletePluginComponents: commit", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) ListPluginUserConfig(ctx context.Context, pluginID string) ([]types.PluginUserConfigRow, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT plugin_id, scope, key, value, sensitive FROM plugin_user_config WHERE plugin_id=? ORDER BY scope, key", pluginID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPluginUserConfig", err)
	}
	defer rows.Close()
	var out []types.PluginUserConfigRow
	for rows.Next() {
		var row types.PluginUserConfigRow
		var sensitive int
		if err := rows.Scan(&row.PluginID, &row.Scope, &row.Key, &row.Value, &sensitive); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPluginUserConfig scan", err)
		}
		row.Sensitive = sensitive == 1
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPluginUserConfig: rows", err)
	}
	return out, nil
}

func (r *SQLiteExtensionRepository) SavePluginUserConfig(ctx context.Context, pluginID string, cfg []types.PluginUserConfigRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SavePluginUserConfig", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, "DELETE FROM plugin_user_config WHERE plugin_id=?", pluginID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SavePluginUserConfig: delete", err)
	}
	for _, row := range cfg {
		sensitive := 0
		if row.Sensitive {
			sensitive = 1
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO plugin_user_config(plugin_id, scope, key, value, sensitive) VALUES(?,?,?,?,?)",
			pluginID, row.Scope, row.Key, row.Value, sensitive); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SavePluginUserConfig: insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SavePluginUserConfig: commit", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) ListPlugins(ctx context.Context) ([]types.PluginRow, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, name, version, display_name, description, publisher, homepage, install_path, enabled, trust_tier, catalog_id, mcp_policy, manifest, created_at, updated_at FROM plugins ORDER BY name")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPlugins", err)
	}
	defer rows.Close()
	var out []types.PluginRow
	for rows.Next() {
		var p types.PluginRow
		var enabled int
		if err := rows.Scan(&p.ID, &p.Name, &p.Version, &p.DisplayName, &p.Description, &p.Publisher, &p.Homepage,
			&p.InstallPath, &enabled, &p.TrustTier, &p.CatalogID, &p.MCPPolicy, &p.Manifest, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPlugins scan", err)
		}
		p.Enabled = enabled == 1
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPlugins: rows", err)
	}
	return out, nil
}

func (r *SQLiteExtensionRepository) ListHookTrust(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT source_key, digest FROM hook_trust")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListHookTrust", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, d string
		if err := rows.Scan(&k, &d); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListHookTrust scan", err)
		}
		out[k] = d
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListHookTrust: rows", err)
	}
	return out, nil
}

func (r *SQLiteExtensionRepository) SaveHookTrust(ctx context.Context, sourceKey, digest string) error {
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO hook_trust(source_key, digest) VALUES(?,?) ON CONFLICT(source_key) DO UPDATE SET digest=excluded.digest, trusted_at=strftime('%Y-%m-%dT%H:%M:%SZ','now')",
		sourceKey, digest)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SaveHookTrust", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) DeleteHookTrust(ctx context.Context, sourceKey string) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM hook_trust WHERE source_key=?", sourceKey); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteHookTrust", err)
	}
	return nil
}

func (r *SQLiteExtensionRepository) ListPluginChannelStates(ctx context.Context) ([]types.PluginChannelState, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT plugin_id, server, enabled, permission_relay FROM plugin_channels")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPluginChannelStates", err)
	}
	defer rows.Close()
	var out []types.PluginChannelState
	for rows.Next() {
		var s types.PluginChannelState
		var enabled, relay int
		if err := rows.Scan(&s.PluginID, &s.Server, &enabled, &relay); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPluginChannelStates scan", err)
		}
		s.Enabled, s.PermissionRelay = enabled == 1, relay == 1
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListPluginChannelStates: rows", err)
	}
	return out, nil
}

func (r *SQLiteExtensionRepository) SavePluginChannelState(ctx context.Context, s types.PluginChannelState) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO plugin_channels(plugin_id, server, enabled, permission_relay) VALUES(?,?,?,?)
		ON CONFLICT(plugin_id, server) DO UPDATE SET enabled=excluded.enabled, permission_relay=excluded.permission_relay,
		updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now')`, s.PluginID, s.Server, boolInt(s.Enabled), boolInt(s.PermissionRelay))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.SavePluginChannelState", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
