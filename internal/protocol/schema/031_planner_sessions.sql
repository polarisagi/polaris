-- 031_planner_sessions: MCTS 规划会话记录
-- 架构角色: PlannerPool 执行记录，用于审计和经验积累
CREATE TABLE IF NOT EXISTS planner_sessions (
    id              TEXT    PRIMARY KEY,         -- "plan_{UUID}"
    task_id         TEXT    NOT NULL DEFAULT '', -- 关联的 tasks.task_id
    goal            TEXT    NOT NULL,
    task_type       TEXT    NOT NULL,            -- 'code_act' | 'general'
    worker_count    INTEGER NOT NULL DEFAULT 3,
    winning_score   REAL    NOT NULL DEFAULT 0.0,
    winning_engine  TEXT    NOT NULL DEFAULT '', -- 'engine_a' | 'engine_b'
    status          TEXT    NOT NULL DEFAULT 'running', -- 'running' | 'done' | 'failed'
    created_at      INTEGER NOT NULL,
    completed_at    INTEGER
) STRICT;

CREATE INDEX IF NOT EXISTS idx_planner_task ON planner_sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_planner_status ON planner_sessions(status, created_at DESC);
