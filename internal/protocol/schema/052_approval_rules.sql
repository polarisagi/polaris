-- ============================================================================
-- 052_approval_rules: 项目级“以后不再询问”授权规则（ADR-0117 决策五）
-- ============================================================================
-- 架构角色: 用户在审批卡勾选“以后不再询问”后落下的最窄规则；policy.Decide 在询问前先匹配，
--           命中即放行并更新 hit_count/last_used_at。硬限制永不读取本表。
-- 关联: M11(Policy & Safety)、projects（删除项目级联清除其规则）
-- ============================================================================

CREATE TABLE IF NOT EXISTS approval_rules (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK(kind IN ('cmd','domain','dir','app','tool')),
    pattern      TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    last_used_at TEXT NOT NULL DEFAULT '',
    hit_count    INTEGER NOT NULL DEFAULT 0,
    UNIQUE(project_id, kind, pattern)
);

CREATE INDEX IF NOT EXISTS idx_approval_rules_project ON approval_rules(project_id);
