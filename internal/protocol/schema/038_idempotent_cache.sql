-- 038_idempotent_cache.sql
-- GovernanceAgent CodeAct 幂等响应缓存（从 outbox 表解耦，修复 GR-11-001）。
-- 独立于 OutboxWorker 的 7d Janitor 清洗周期，由 GovernanceAgent 自行管理 TTL。
CREATE TABLE IF NOT EXISTS idempotent_cache (
    operation_hash TEXT PRIMARY KEY,
    payload        TEXT    NOT NULL,
    created_at     INTEGER NOT NULL  -- Unix 毫秒时间戳
);

-- 30 天过期清理索引
CREATE INDEX IF NOT EXISTS idx_idempotent_cache_created_at ON idempotent_cache(created_at);
