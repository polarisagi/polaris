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
		`SELECT id, name, transport, command, args, env, url, headers, oauth, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, requires_network, created_at, updated_at
		FROM mcp_servers ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.ListMCPServers", err)
	}
	defer rows.Close()

	var result []types.MCPServerRow
	for rows.Next() {
		var row types.MCPServerRow
		var enabledInt, requiresNetworkInt int
		if err := rows.Scan(&row.ID, &row.Name, &row.Transport, &row.Command, &row.Args, &row.Env, &row.URL, &row.Headers, &row.OAuth, &enabledInt, &row.Timeout, &row.TrustTier, &row.CatalogID, &row.PluginID, &row.WorkDir, &requiresNetworkInt, &row.CreatedAt, &row.UpdatedAt); err != nil {
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
		`SELECT id, name, transport, command, args, env, url, headers, oauth, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, requires_network, created_at, updated_at
		FROM mcp_servers WHERE id=?`, id).Scan(
		&row.ID, &row.Name, &row.Transport, &row.Command, &row.Args, &row.Env, &row.URL, &row.Headers, &row.OAuth, &enabledInt, &row.Timeout, &row.TrustTier, &row.CatalogID, &row.PluginID, &row.WorkDir, &requiresNetworkInt, &row.CreatedAt, &row.UpdatedAt)
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
		`INSERT INTO mcp_servers(id, name, transport, command, args, env, url, headers, oauth, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, requires_network, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, transport=excluded.transport, command=excluded.command,
		  args=excluded.args, env=excluded.env, url=excluded.url, headers=excluded.headers, oauth=excluded.oauth, enabled=excluded.enabled,
		  timeout=excluded.timeout, trust_tier=excluded.trust_tier, work_dir=excluded.work_dir,
		  requires_network=excluded.requires_network, updated_at=excluded.updated_at`,
		row.ID, row.Name, row.Transport, row.Command, row.Args, row.Env, row.URL, headersOrEmpty(row.Headers), jsonObjOrEmpty(row.OAuth), enabledInt, row.Timeout, row.TrustTier, row.CatalogID, row.PluginID, row.WorkDir, requiresNetworkInt, row.CreatedAt, row.UpdatedAt)
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

// DeleteMCPServer 删除服务器行；同一事务内删除其 mcp_oauth_tokens 令牌——
// mcp_servers 行是令牌 server_id 的权威源，SQLite 未启用外键级联，需显式清理，
// 否则孤儿令牌会在服务器 ID 被复用（如重装同名连接器）时被错误地读取到。
func (r *SQLiteExtensionRepository) DeleteMCPServer(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteMCPServer begin", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_oauth_tokens WHERE server_id=?`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteMCPServer token", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id=?`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteMCPServer", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteMCPServer commit", err)
	}
	return nil
}

// --- mcp_oauth_clients ---

// GetMCPOAuthClient 按 (issuer, redirect_uri) 精确查找 DCR 动态注册客户端；不存在返回 nil。
func (r *SQLiteExtensionRepository) GetMCPOAuthClient(ctx context.Context, issuer, redirectURI string) (*types.MCPOAuthClientRow, error) {
	var row types.MCPOAuthClientRow
	err := r.db.QueryRowContext(ctx,
		`SELECT issuer, redirect_uri, client_id, client_secret_enc, registration_method, created_at
		FROM mcp_oauth_clients WHERE issuer=? AND redirect_uri=?`, issuer, redirectURI).Scan(
		&row.Issuer, &row.RedirectURI, &row.ClientID, &row.ClientSecretEnc, &row.RegistrationMethod, &row.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetMCPOAuthClient", err)
	}
	return &row, nil
}

// UpsertMCPOAuthClient 写入/更新一次 DCR 动态注册结果。
func (r *SQLiteExtensionRepository) UpsertMCPOAuthClient(ctx context.Context, row types.MCPOAuthClientRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_clients(issuer, redirect_uri, client_id, client_secret_enc, registration_method, created_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(issuer, redirect_uri) DO UPDATE SET
		  client_id=excluded.client_id, client_secret_enc=excluded.client_secret_enc,
		  registration_method=excluded.registration_method`,
		row.Issuer, row.RedirectURI, row.ClientID, row.ClientSecretEnc, row.RegistrationMethod, row.CreatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpsertMCPOAuthClient", err)
	}
	return nil
}

// --- mcp_oauth_tokens ---

// GetMCPOAuthToken 按 server_id 查找当前令牌；不存在返回 nil。
func (r *SQLiteExtensionRepository) GetMCPOAuthToken(ctx context.Context, serverID string) (*types.MCPOAuthTokenRow, error) {
	var row types.MCPOAuthTokenRow
	err := r.db.QueryRowContext(ctx,
		`SELECT server_id, issuer, resource, client_id, redirect_uri, access_token_enc, refresh_token_enc, token_type, scopes, expires_at, updated_at
		FROM mcp_oauth_tokens WHERE server_id=?`, serverID).Scan(
		&row.ServerID, &row.Issuer, &row.Resource, &row.ClientID, &row.RedirectURI, &row.AccessTokenEnc, &row.RefreshTokenEnc, &row.TokenType, &row.Scopes, &row.ExpiresAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.GetMCPOAuthToken", err)
	}
	return &row, nil
}

// UpsertMCPOAuthToken 写入/更新一个 MCP Server 的令牌（首次授权与刷新共用本方法）。
func (r *SQLiteExtensionRepository) UpsertMCPOAuthToken(ctx context.Context, row types.MCPOAuthTokenRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_tokens(server_id, issuer, resource, client_id, redirect_uri, access_token_enc, refresh_token_enc, token_type, scopes, expires_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(server_id) DO UPDATE SET
		  issuer=excluded.issuer, resource=excluded.resource, client_id=excluded.client_id,
		  redirect_uri=excluded.redirect_uri, access_token_enc=excluded.access_token_enc,
		  refresh_token_enc=excluded.refresh_token_enc, token_type=excluded.token_type, scopes=excluded.scopes,
		  expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		row.ServerID, row.Issuer, row.Resource, row.ClientID, row.RedirectURI, row.AccessTokenEnc, row.RefreshTokenEnc, row.TokenType, row.Scopes, row.ExpiresAt, row.UpdatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UpsertMCPOAuthToken", err)
	}
	return nil
}

// DeleteMCPOAuthToken 删除一个 MCP Server 的令牌（授权服务器变更时丢弃旧令牌，见 basic_authorization_client-registration.md）。
func (r *SQLiteExtensionRepository) DeleteMCPOAuthToken(ctx context.Context, serverID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM mcp_oauth_tokens WHERE server_id=?`, serverID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.DeleteMCPOAuthToken", err)
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
		if err := uninstallCleanupMCP(ctx, tx, id, runtimeID); err != nil {
			return err
		}
	case "native", "plugin":
		if err := uninstallCleanupPlugin(ctx, tx, id); err != nil {
			return err
		}
	case "skill":
		if err := uninstallCleanupSkill(ctx, tx, runtimeID); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup commit", err)
	}
	return nil
}

// uninstallCleanupMCP 独立连接器卸载：令牌先于服务器行删除（DELETE 子查询依赖 mcp_servers
// 行仍然存在才能定位 server_id）。
func uninstallCleanupMCP(ctx context.Context, tx *sql.Tx, id, runtimeID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_oauth_tokens WHERE server_id IN (SELECT id FROM mcp_servers WHERE plugin_id=? OR id=?)`, id, runtimeID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup mcp oauth token", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE plugin_id=? OR id=?`, id, runtimeID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup mcp", err)
	}
	return nil
}

// uninstallCleanupPlugin 插件卸载：子 MCP 服务器（含其 OAuth 令牌）+ 技能 + 插件行本身。
func uninstallCleanupPlugin(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_oauth_tokens WHERE server_id IN (SELECT id FROM mcp_servers WHERE plugin_id=?)`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup plugin mcp oauth token", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE plugin_id=?`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup plugin mcp", err)
	}
	// 技能动态注入信任随技能删除（须在删 skills 行之前按 plugin_id 定位）。
	if _, err := tx.ExecContext(ctx, `DELETE FROM hook_trust WHERE source_key IN (SELECT 'skill:' || name FROM skills WHERE plugin_id=?)`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skill trust", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skills WHERE plugin_id=?`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skills", err)
	}
	// plugins.name 唯一：不删行则同名插件无法重装；用户配置（含密文）随插件一并删除。
	for _, q := range []string{`DELETE FROM plugins WHERE id=?`, `DELETE FROM plugin_user_config WHERE plugin_id=?`,
		`DELETE FROM plugin_channels WHERE plugin_id=?`, `DELETE FROM plugin_app_bindings WHERE plugin_id=?`} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup plugins", err)
		}
	}
	// hook 信任随插件删除：重装后须重新审阅。
	if _, err := tx.ExecContext(ctx, `DELETE FROM hook_trust WHERE source_key LIKE ?`, "plugin:"+id+":%"); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup hook_trust", err)
	}
	return nil
}

func uninstallCleanupSkill(ctx context.Context, tx *sql.Tx, runtimeID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM skills WHERE name=? AND plugin_id=''`, runtimeID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skill", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM hook_trust WHERE source_key=?`, "skill:"+runtimeID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteExtensionRepository.UninstallCleanup skill trust", err)
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

// jsonObjOrEmpty 与 headersOrEmpty 同一用途，供 oauth 列复用（DDL 约束 NOT NULL DEFAULT '{}'）。
func jsonObjOrEmpty(v string) string {
	if v == "" {
		return "{}"
	}
	return v
}
