-- ============================================================================
-- 016_preferences: 系统偏好设置（全局 KV 存储）
-- ============================================================================

CREATE TABLE IF NOT EXISTS preferences (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
