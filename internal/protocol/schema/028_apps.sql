-- ============================================================================
-- 028_apps: 富交互应用（App）运行时配置表
-- ============================================================================
-- 架构角色: 对应 Codex Apps / ChatGPT Apps SDK 概念。
--   App 是独立于 MCP 工具和文本 Skill 的富交互前端扩展，为大模型或用户提供独立的
--   UI Widget（组件）、工作流视图或独立的 Web Endpoint。
--   与 MCP（纯后端无头服务）不同，App 拥有前端路由与用户状态交互能力。
--
-- 消费方:
--   Gateway/UI 层 - 将大模型的输出渲染为 App 的 Custom Widget，或在独立标签页打开 App 视图。
--   MCPManager   - 关联 App 背后支撑状态的 MCP 服务，处理 OAuth/认证流。
--
-- 关联: extension_instances.runtime_id → apps.id (ext_type='app')
-- ============================================================================

CREATE TABLE IF NOT EXISTS apps (
    id           TEXT    PRIMARY KEY,           -- "app_{8hex}"
    name         TEXT    NOT NULL UNIQUE,        -- App 唯一名称
    display_name TEXT    NOT NULL DEFAULT '',    -- 界面显示名称
    description  TEXT    NOT NULL DEFAULT '',
    url          TEXT    NOT NULL,               -- App 的 Web UI 入口或 Widget 宿主端点
    publisher    TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,     -- 运行时开关
    trust_tier   INTEGER NOT NULL DEFAULT 1,     -- 继承自 extension_instances
    catalog_id   TEXT    NOT NULL DEFAULT '',    -- 关联 extension_catalog.id
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_apps_enabled ON apps(enabled);
CREATE INDEX IF NOT EXISTS idx_apps_catalog ON apps(catalog_id) WHERE catalog_id != '';
