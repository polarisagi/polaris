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
    retry_count              INTEGER NOT NULL DEFAULT 0,
    max_retries              INTEGER NOT NULL DEFAULT 3,
    depends_on               TEXT,
    error                    TEXT,
    suspend_reason           TEXT,
    pii_vault_blob           TEXT,
    provider_suspended_count INTEGER NOT NULL DEFAULT 0,
    -- TaintLevel: 0=TaintNone, 1=TaintLow, 2=TaintMedium, 3=TaintHigh, 4=TaintUserReviewed
    -- 随 Intent/Result 跨 Agent 边界传递（inv_M8_05），只升不降
    intent_taint             INTEGER NOT NULL DEFAULT 0,
    result_taint             INTEGER NOT NULL DEFAULT 0,
    -- 流水线阶段 handoff 字段（M08 §5 Pipeline Protocol）
    -- pipeline_id: 所属流水线实例 ID，空表示非流水线任务
    pipeline_id              TEXT,
    -- pipeline_stage: 阶段名称（research/plan/execute/verify）
    pipeline_stage           TEXT,
    -- context_payload: 前序阶段结构化产出（JSON），Agent S_PERCEIVE 优先读取
    context_payload          TEXT,
    -- Token 记账（Gap-A: per-task observability，HE-Rule-1）
    -- tokens_input/output: 本任务累计 LLM 输入/输出 token 数
    -- tokens_cache_read: Anthropic prompt cache 命中 token 数（缓存命中比输入便宜 10x）
    -- cost_usd: 本任务估算费用（= input/1K × CostPer1KInput + output/1K × CostPer1KOutput）
    tokens_input             INTEGER NOT NULL DEFAULT 0,
    tokens_output            INTEGER NOT NULL DEFAULT 0,
    tokens_cache_read        INTEGER NOT NULL DEFAULT 0,
    cost_usd                 REAL    NOT NULL DEFAULT 0.0,
    created_at               TEXT    NOT NULL,
    updated_at               TEXT    NOT NULL
);

-- Reaper 轮询超时过期任务（status + expires_at 复合过滤）
CREATE INDEX IF NOT EXISTS idx_tasks_reaper
    ON tasks(status, expires_at) WHERE expires_at IS NOT NULL;

-- 流水线状态查询索引（按 pipeline_id + stage 快速定位）
CREATE INDEX IF NOT EXISTS idx_tasks_pipeline
    ON tasks(pipeline_id, pipeline_stage) WHERE pipeline_id IS NOT NULL;
