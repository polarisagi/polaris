-- ============================================================================
-- 014_cron_jobs: 定时任务持久化
-- schedule 格式: @hourly | @daily | @weekly | @every <N>m | @every <N>h
-- ============================================================================

CREATE TABLE IF NOT EXISTS cron_jobs (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL DEFAULT '',
    prompt      TEXT    NOT NULL,
    schedule    TEXT    NOT NULL,
    session_id  TEXT,                         -- NULL=每次新建会话
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run_at TEXT,                         -- ISO8601，NULL=从未执行
    next_run_at TEXT    NOT NULL,             -- ISO8601，调度器维护
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_cron_next ON cron_jobs(enabled, next_run_at);
