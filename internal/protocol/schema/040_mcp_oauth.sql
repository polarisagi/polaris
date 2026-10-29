-- ============================================================================
-- 040_mcp_oauth: MCP OAuth 客户端引擎运行期状态（MCP 2026-07-28 basic_authorization.md）
-- ============================================================================
-- 架构角色: 预注册配置（client_id/client_secret_enc/auth_server_metadata_url/scopes）存于
-- mcp_servers.oauth 列（015）；本文件只存"运行期产生"的两类状态：
--   1) mcp_oauth_clients: Dynamic Client Registration（DCR）动态注册得到的客户端。
--   2) mcp_oauth_tokens:  每个 MCP Server 当前持有的已获取令牌。
-- 关联: M13-bis(Extension Registry), internal/extension/mcp（OAuth 引擎）
-- ============================================================================

-- mcp_oauth_clients：规范要求客户端凭据必须按签发它们的授权服务器 issuer 绑定，
-- 授权服务器变更后不得复用旧凭据（basic_authorization_client-registration.md
-- §Authorization Server Binding）。主键 (issuer, redirect_uri) 天然满足这一隔离——
-- issuer 变化后查询自然查不到旧客户端，无需显式失效逻辑。
CREATE TABLE IF NOT EXISTS mcp_oauth_clients (
    issuer              TEXT NOT NULL,
    redirect_uri        TEXT NOT NULL,
    client_id           TEXT NOT NULL,
    client_secret_enc   TEXT NOT NULL DEFAULT '',   -- credential.Vault 密文；公开客户端为空
    registration_method TEXT NOT NULL DEFAULT 'dcr',
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    PRIMARY KEY (issuer, redirect_uri)
);

-- mcp_oauth_tokens：每个 MCP Server 当前持有的令牌（一对一）。server_id 随 mcp_servers 行
-- 删除级联清理——由 repo.DeleteMCPServer / UninstallCleanup 显式一并删除
-- （SQLite 默认不启用外键级联，且上线前阶段不引入 PRAGMA foreign_keys 依赖）。
CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (
    server_id          TEXT PRIMARY KEY,
    issuer             TEXT NOT NULL,
    resource           TEXT NOT NULL DEFAULT '',
    -- 刷新令牌请求须携带签发时的 client_id（OAuth 2.1 §4.3.1，公开客户端必填）；
    -- redirect_uri 与 issuer 一起定位 DCR 客户端的 client_secret（mcp_oauth_clients 主键）。
    client_id          TEXT NOT NULL DEFAULT '',
    redirect_uri       TEXT NOT NULL DEFAULT '',
    access_token_enc   TEXT NOT NULL DEFAULT '',    -- credential.Vault 密文
    refresh_token_enc  TEXT NOT NULL DEFAULT '',    -- credential.Vault 密文；可空
    token_type         TEXT NOT NULL DEFAULT 'Bearer',
    scopes             TEXT NOT NULL DEFAULT '',    -- 空格分隔，已请求 scope 集合的并集（step-up 用）
    expires_at         TEXT NOT NULL DEFAULT '',    -- RFC3339；空=未知/不过期
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
