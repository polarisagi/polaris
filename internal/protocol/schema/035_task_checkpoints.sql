-- 断点续传记录表，用于 StateGraphExecutor 节点级别的容错恢复
CREATE TABLE task_checkpoints (
    task_id       TEXT NOT NULL,
    node_id       TEXT NOT NULL,
    attempt       INTEGER NOT NULL DEFAULT 1,
    status        TEXT NOT NULL,        -- pending|executing|done|failed
    output_json   TEXT,                 -- NodeResult 序列化
    idempotency_key TEXT,               -- 幂等键
    taint_level   INTEGER NOT NULL,
    started_at    INTEGER,
    completed_at  INTEGER,
    error         TEXT,
    PRIMARY KEY (task_id, node_id, attempt)
);
CREATE INDEX idx_task_checkpoints_task ON task_checkpoints(task_id);
