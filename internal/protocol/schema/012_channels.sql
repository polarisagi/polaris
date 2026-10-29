-- ============================================================================
-- 012_channels: 聊天平台集成配置（M13 adapter 框架）
-- ============================================================================

CREATE TABLE IF NOT EXISTS channels (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    type           TEXT NOT NULL CHECK(type IN ('telegram','feishu','slack','discord','webhook')),
    enabled        INTEGER NOT NULL DEFAULT 1,
    config_json    TEXT NOT NULL DEFAULT '{}',
    webhook_secret TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_channels_type    ON channels(type);
CREATE INDEX IF NOT EXISTS idx_channels_enabled ON channels(enabled);
