-- ============================================================================
-- 051_agent_loop_rounds: Agentic Loop 轮次增量持久化（R2-05 / ADR-0104）
-- ============================================================================
-- 架构角色: 记录 Agentic Loop 每轮的新增增量消息与元数据，用于崩溃无损续跑与轨迹回放。
-- 关联: M4(Agent Kernel)
-- ============================================================================

CREATE TABLE IF NOT EXISTS agent_loop_rounds (
    session_id  TEXT NOT NULL,
    turn_id     TEXT NOT NULL,
    round       INTEGER NOT NULL,
    status      TEXT NOT NULL, -- running | done | aborted
    delta_json  TEXT NOT NULL,
    taint_level INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (session_id, turn_id, round)
);

CREATE INDEX IF NOT EXISTS idx_agent_loop_rounds_status ON agent_loop_rounds(status);
