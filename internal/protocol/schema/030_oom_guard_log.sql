-- 030_oom_guard_log: OOM Guard 触发记录
-- 用于审计内存压力事件，不用于业务逻辑
CREATE TABLE IF NOT EXISTS oom_guard_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    triggered_at INTEGER NOT NULL,  -- Unix 毫秒
    free_mem_mb  INTEGER NOT NULL,  -- 触发时的空闲内存 MB
    total_mem_mb INTEGER NOT NULL,
    action       TEXT    NOT NULL   -- 'pause_surreal_write' | 'resume'
);
CREATE INDEX IF NOT EXISTS idx_oom_guard_time ON oom_guard_log(triggered_at DESC);
