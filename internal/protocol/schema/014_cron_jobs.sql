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
    -- 电路断路器字段（Gap-C, HE-Rule-2）
    -- failure_count: 连续失败次数；success 时归零
    -- circuit_open: 1=断路（达阈值后调度器跳过），0=正常
    -- last_error: 最近一次失败的错误消息快照
    -- circuit_opened_at: 断路时间戳（ISO8601）
    failure_count       INTEGER NOT NULL DEFAULT 0,
    circuit_open        INTEGER NOT NULL DEFAULT 0,
    last_error          TEXT    NOT NULL DEFAULT '',
    circuit_opened_at   TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- 调度器只轮询 enabled=1 且未断路的任务
CREATE INDEX IF NOT EXISTS idx_cron_next ON cron_jobs(enabled, circuit_open, next_run_at);
