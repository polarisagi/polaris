package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// mcp_servers 表操作 + 卸载清理（R7 拆分自 repo_extension.go）。
// 结构体/构造函数/extension_instances/extension_catalog 见 repo_extension.go；
// plugins 表操作见 repo_extension_plugins.go。
// ============================================================================

// --- mcp_servers ---

func (r *SQLiteExtensionRepository) ListMCPServers(ctx context.Context) ([]types.MCPServerRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, transport, command, args, env, url, headers, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, requires_network, created_at, updated_at
		FROM mcp_servers ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListMCPServers", err)
	}
	defer rows.Close()

	var result []types.MCPServerRow
	for rows.Next() {
		var row types.MCPServerRow
		var enabledInt, requiresNetworkInt int
		if err := rows.Scan(&row.ID, &row.Name, &row.Transport, &row.Command, &row.Args, &row.Env, &row.URL, &row.Headers, &enabledInt, &row.Timeout, &row.TrustTier, &row.CatalogID, &row.PluginID, &row.WorkDir, &requiresNetworkInt, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListMCPServers scan", err)
		}
		row.Enabled = enabledInt == 1
		row.RequiresNetwork = requiresNetworkInt == 1
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListMCPServers: rows iteration", err)
	}
	return result, nil
}

func (r *SQLiteExtensionRepository) GetMCPServer(ctx context.Context, id string) (*types.MCPServerRow, error) {
	var row types.MCPServerRow
	var enabledInt, requiresNetworkInt int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, transport, command, args, env, url, headers, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, requires_network, created_at, updated_at
		FROM mcp_servers WHERE id=?`, id).Scan(
		&row.ID, &row.Name, &row.Transport, &row.Command, &row.Args, &row.Env, &row.URL, &row.Headers, &enabledInt, &row.Timeout, &row.TrustTier, &row.CatalogID, &row.PluginID, &row.WorkDir, &requiresNetworkInt, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetMCPServer", err)
	}
	row.Enabled = enabledInt == 1
	row.RequiresNetwork = requiresNetworkInt == 1
	return &row, nil
}

func (r *SQLiteExtensionRepository) UpsertMCPServer(ctx context.Context, row types.MCPServerRow) error {
	enabledInt := 0
	if row.Enabled {
		enabledInt = 1
	}
	requiresNetworkInt := 0
	if row.RequiresNetwork {
		requiresNetworkInt = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO mcp_servers(id, name, transport, command, args, env, url, headers, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, requires_network, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, transport=excluded.transport, command=excluded.command,
		  args=excluded.args, env=excluded.env, url=excluded.url, headers=excluded.headers, enabled=excluded.enabled,
		  timeout=excluded.timeout, trust_tier=excluded.trust_tier, work_dir=excluded.work_dir,
		  requires_network=excluded.requires_network, updated_at=excluded.updated_at`,
		row.ID, row.Name, row.Transport, row.Command, row.Args, row.Env, row.URL, headersOrEmpty(row.Headers), enabledInt, row.Timeout, row.TrustTier, row.CatalogID, row.PluginID, row.WorkDir, requiresNetworkInt, row.CreatedAt, row.UpdatedAt)
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

	switch extType {
	case "mcp":
		_, err = tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE plugin_id=? OR id=?`, id, runtimeID)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup mcp", err)
		}
	case "native", "plugin":
		_, err = tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE plugin_id=?`, id)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup plugin mcp", err)
		}
		// 技能动态注入信任随技能删除（须在删 skills 行之前按 plugin_id 定位）。
		_, err = tx.ExecContext(ctx, `DELETE FROM hook_trust WHERE source_key IN (SELECT 'skill:' || name FROM skills WHERE plugin_id=?)`, id)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skill trust", err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM skills WHERE plugin_id=?`, id)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skills", err)
		}
		// plugins.name 唯一：不删行则同名插件无法重装；用户配置（含密文）随插件一并删除。
		for _, q := range []string{`DELETE FROM plugins WHERE id=?`, `DELETE FROM plugin_user_config WHERE plugin_id=?`} {
			if _, err = tx.ExecContext(ctx, q, id); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup plugins", err)
			}
		}
		// hook 信任随插件删除：重装后须重新审阅。
		if _, err = tx.ExecContext(ctx, `DELETE FROM hook_trust WHERE source_key LIKE ?`, "plugin:"+id+":%"); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup hook_trust", err)
		}
	case "skill":
		_, err = tx.ExecContext(ctx, `DELETE FROM skills WHERE name=? AND plugin_id=''`, runtimeID)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skill", err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM hook_trust WHERE source_key=?`, "skill:"+runtimeID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skill trust", err)
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

func headersOrEmpty(h string) string {
	if h == "" {
		return "{}"
	}
	return h
}
