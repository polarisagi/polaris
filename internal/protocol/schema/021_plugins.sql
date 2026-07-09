-- ============================================================================
-- 021_plugins: 已安装插件的运行时状态表
-- ============================================================================
-- 架构角色: 对应 Codex config.toml 的 [plugins."xxx"] 配置段。
--   每条记录 = 一个已安装插件的运行时状态。
--
-- 三层统一架构（agentskills.io 标准，对齐 OpenAI Codex / Claude Code）：
--   插件安装时，其所有子组件写入各自的全局运行时表，通过 plugin_id FK 关联：
--     - 子 MCP  → mcp_servers（plugin_id=plugins.id, work_dir=install_path）
--     - 子 Skill → skills（plugin_id=plugins.id）
--   插件禁用时：级联 mcp_servers.enabled=0 + skills.deprecated=1
--   插件启用时：级联 mcp_servers.enabled=1 + skills.deprecated=0
--   插件卸载时：DELETE FROM mcp_servers WHERE plugin_id + DELETE FROM skills WHERE plugin_id
--
-- mcp_policy 仅保留每个子 MCP 的额外策略（approval_mode、enabled_tools），
-- 子 MCP 的 enabled 状态以 mcp_servers.enabled 为权威（mcp_policy 中的 enabled 废弃）。
--
-- 消费方:
--   MCPManager    - RestoreServersFromDB() 统一加载（含 plugin_id 非空的子 MCP）
--   SSE handler   - SELECT * FROM skills WHERE exec_mode='ambient' AND deprecated=0
--   UI/API 层     - GET /v1/plugins 展示已安装插件列表
-- 关联: extension_instances.runtime_id → plugins.id (ext_type='plugin')
-- ============================================================================

CREATE TABLE IF NOT EXISTS plugins (
    id           TEXT    PRIMARY KEY,           -- "pl_{8hex}"
    name         TEXT    NOT NULL UNIQUE,        -- kebab-case，Codex plugin namespace
    version      TEXT    NOT NULL DEFAULT '1.0.0',
    display_name TEXT    NOT NULL DEFAULT '',    -- interface.displayName
    description  TEXT    NOT NULL DEFAULT '',
    publisher    TEXT    NOT NULL DEFAULT '',    -- author.name / plugin.json author.name
    homepage     TEXT    NOT NULL DEFAULT '',    -- plugin.json homepage
    install_path TEXT    NOT NULL DEFAULT '',    -- 插件根目录绝对路径，运行时加载唯一来源
    enabled      INTEGER NOT NULL DEFAULT 1,     -- 全局开关；0 时 MCPManager/SSE 跳过此插件
    trust_tier   INTEGER NOT NULL DEFAULT 1,     -- 0-4，继承自 extension_instances
    catalog_id   TEXT    NOT NULL DEFAULT '',    -- extension_catalog.id；用户手动安装时为空
    -- 子 MCP 运行时策略（对应 Codex [plugins.xxx.mcp_servers.yyy]）
    -- JSON map: { "server-name": { "enabled": true, "approval_mode": "prompt", "enabled_tools": ["x"] } }
    mcp_policy   TEXT    NOT NULL DEFAULT '{}',
    -- plugin.json 完整快照（运行时缓存；权威来源始终是 install_path/.polaris-plugin/plugin.json）
    manifest     TEXT    NOT NULL DEFAULT '{}',
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_plugins_enabled ON plugins(enabled);
CREATE INDEX IF NOT EXISTS idx_plugins_catalog ON plugins(catalog_id) WHERE catalog_id != '';
CREATE INDEX IF NOT EXISTS idx_plugins_trust   ON plugins(trust_tier);
