-- ============================================================================
-- 053_agent_goals: 跨回合目标（ADR-0120 决策七）
-- ============================================================================
-- 架构角色: goal 工具持久化的会话级目标；回合收尾时网关据此自动发起续跑回合。
--           每会话至多一个 active 目标（部分唯一索引兜底）。
-- 关联: M04(Agent Kernel)、ADR-0116(回合与连接解耦)、ADR-0120
-- ============================================================================

CREATE TABLE IF NOT EXISTS agent_goals (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL,
    objective     TEXT NOT NULL,
    done_criteria TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL CHECK(status IN ('active','paused','blocked','complete')),
    note          TEXT NOT NULL DEFAULT '',
    max_rounds    INTEGER NOT NULL DEFAULT 20,
    rounds_used   INTEGER NOT NULL DEFAULT 0,
    idle_rounds   INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_goals_session_status ON agent_goals(session_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_goals_one_active ON agent_goals(session_id) WHERE status = 'active';
