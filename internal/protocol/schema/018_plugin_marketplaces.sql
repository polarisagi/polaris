-- ============================================================================
-- 018_plugin_marketplaces: 扩展市场来源注册（Layer 0 Market）
-- ============================================================================
-- 架构角色: 记录市场来源配置。内置 4 条（is_builtin=1，不可删除），用户可追加。
-- 关联: M13-bis(Extension Registry) §1
-- ============================================================================

CREATE TABLE IF NOT EXISTS plugin_marketplaces (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL,
    type        TEXT    NOT NULL,   -- 'skill' | 'mcp' | 'plugin' | 'app'
    publisher   TEXT    NOT NULL,
    repo_url    TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    is_builtin  INTEGER NOT NULL DEFAULT 0,  -- 1=内置，不可删除
    trust_tier  INTEGER NOT NULL DEFAULT 2,  -- 3=Official, 2=Community
    enabled     INTEGER NOT NULL DEFAULT 1,
    sort_order  INTEGER NOT NULL DEFAULT 999, -- 展示排序权重：值越小越靠前；内置市场 0~99，用户新增从 100 起
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
