-- 断点续传记录表，用于 StateGraphExecutor 节点级别的容错恢复
-- 以及 Agent Kernel 委派挂起（S_AWAIT_AGENT）的执行期上下文恢复（GD-13-009）。
CREATE TABLE task_checkpoints (
    task_id       TEXT NOT NULL,
    node_id       TEXT NOT NULL,
    attempt       INTEGER NOT NULL DEFAULT 1,
    status        TEXT NOT NULL,        -- pending|executing|done|failed|await_agent
    output_json   TEXT,                 -- NodeResult 序列化（StateGraph 语义，勿混用）
    idempotency_key TEXT,               -- 幂等键
    taint_level   INTEGER NOT NULL,
    started_at    INTEGER,
    completed_at  INTEGER,
    error         TEXT,
    reason        TEXT,

    resume_ctx_json TEXT,
    -- ↑ Agent 执行期上下文快照（GD-13-009）。仅 reason='handoff_wait' 的行填充。
    --   载荷为 agent.HandoffResumeContext 的 JSON，含 schema_version 用于向后兼容。
    --   与 output_json 严格分离：后者是 StateGraph 的 NodeResult，语义不同，禁止复用。
    --   反序列化后必须重跑 S_VALIDATE 四层校验（防 DB 直改 DAG 提权，见 XR/HE-2）。

    PRIMARY KEY (task_id, node_id, attempt)
);
CREATE INDEX idx_task_checkpoints_task ON task_checkpoints(task_id);
