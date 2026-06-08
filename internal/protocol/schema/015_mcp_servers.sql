-- ============================================================================
-- 015_mcp_servers: MCP Server 连接配置（MCPManager 唯一消费方）
-- ============================================================================
-- 架构角色: 记录 MCP 进程的运行时连接参数。安装来源见 extension_instances（020）。
-- trust_tier 决定 Taint 传播级别（ADR-0016 §2.1, ADR-0018）：
--   3=Official（catalog_id 非空）→ TaintMedium
--   2=Community → TaintHigh
--   1=Local（用户手动） → TaintHigh + 每次提示
-- 关联: M7(Tool/Action Layer), M13-bis(Extension Registry)
-- ============================================================================

CREATE TABLE IF NOT EXISTS mcp_servers (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL,
    transport   TEXT    NOT NULL DEFAULT 'stdio',  -- 'stdio' | 'sse' | 'streamable_http'
    command     TEXT    NOT NULL DEFAULT '',        -- stdio: 可执行命令
    args        TEXT    NOT NULL DEFAULT '[]',      -- stdio: JSON array
    env         TEXT    NOT NULL DEFAULT '{}',      -- stdio: JSON object
    url         TEXT    NOT NULL DEFAULT '',        -- sse / streamable_http 端点
    enabled     INTEGER NOT NULL DEFAULT 1,
    timeout     INTEGER NOT NULL DEFAULT 30,        -- 单次请求超时（秒）
    trust_tier  INTEGER NOT NULL DEFAULT 2,         -- 0-4，见上方说明
    catalog_id  TEXT    NOT NULL DEFAULT '',        -- 关联 extension_catalog.id；用户手动配置时为空
    -- 来自插件 bundle 的 MCP 标注其所属插件 ID（plugins.id = "pl_xxx"）；独立安装时为空。
    -- 插件卸载时级联删除，插件禁用/启用时级联 enabled 同步。
    plugin_id   TEXT    NOT NULL DEFAULT '',
    -- stdio 进程工作目录；插件 MCP 设为 install_path，独立 MCP 留空（继承父进程 cwd）。
    work_dir    TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_mcp_enabled   ON mcp_servers(enabled);
CREATE INDEX IF NOT EXISTS idx_mcp_catalog   ON mcp_servers(catalog_id) WHERE catalog_id != '';
CREATE INDEX IF NOT EXISTS idx_mcp_trust     ON mcp_servers(trust_tier);
CREATE INDEX IF NOT EXISTS idx_mcp_plugin_id ON mcp_servers(plugin_id) WHERE plugin_id != '';
