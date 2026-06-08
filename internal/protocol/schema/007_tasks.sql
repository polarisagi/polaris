-- ============================================================================
-- 007_tasks: Agent 任务生命周期状态
-- ============================================================================
-- 架构角色: 记录多 Agent 任务的完整生命周期。与 events 表和 Blackboard 联动。
-- 关联: M4(Agent Kernel), M8(Multi-Agent Orchestrator)
-- ============================================================================

CREATE TABLE IF NOT EXISTS tasks (
    task_id                  TEXT    PRIMARY KEY,
    session_id               TEXT    NOT NULL,
    status                   TEXT    NOT NULL,
    priority                 INTEGER NOT NULL DEFAULT 1,
    claimed_by               TEXT,
    claimed_at               TEXT,
    expires_at               TEXT,
    version                  INTEGER NOT NULL DEFAULT 0,
    replan_count             INTEGER NOT NULL DEFAULT 0,
    toxicity                 INTEGER NOT NULL DEFAULT 0,
    depends_on               TEXT,
    suspend_reason           TEXT,
    pii_vault_blob           TEXT,
    provider_suspended_count INTEGER NOT NULL DEFAULT 0,
    -- TaintLevel: 0=TaintNone, 1=TaintLow, 2=TaintMedium, 3=TaintHigh, 4=TaintUserReviewed
    -- 随 Intent/Result 跨 Agent 边界传递（inv_M8_05），只升不降
    intent_taint             INTEGER NOT NULL DEFAULT 0,
    result_taint             INTEGER NOT NULL DEFAULT 0,
    created_at               TEXT    NOT NULL,
    updated_at               TEXT    NOT NULL
);

-- Reaper 轮询超时过期任务（status + expires_at 复合过滤）
CREATE INDEX IF NOT EXISTS idx_tasks_reaper
    ON tasks(status, expires_at) WHERE expires_at IS NOT NULL;
