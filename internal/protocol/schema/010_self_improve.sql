-- ============================================================================
-- 010_self_improve: M9 自演化引擎数据层
-- ============================================================================
-- 关联: M9(Self-Improvement Engine)
-- ============================================================================

-- 谬误记录（MEMF）：失败轨迹 + 归因
CREATE TABLE IF NOT EXISTS fallacy_records (
    id                 TEXT    PRIMARY KEY,
    task_type          TEXT    NOT NULL,
    failure_type       TEXT    NOT NULL,
    keywords_json      TEXT    NOT NULL DEFAULT '[]',
    reflection         TEXT    NOT NULL,
    occurrence_count   INTEGER NOT NULL DEFAULT 1,
    node_quality_score REAL    NOT NULL DEFAULT 1.0,
    created_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_fallacy_task_type ON fallacy_records(task_type);
CREATE INDEX IF NOT EXISTS idx_fallacy_score     ON fallacy_records(node_quality_score);

-- 启发式成功规则（Heuristics Memory）
CREATE TABLE IF NOT EXISTS heuristics_memory (
    id           TEXT  PRIMARY KEY,
    task_type    TEXT  NOT NULL,
    content      TEXT  NOT NULL,
    success_rate REAL  NOT NULL DEFAULT 1.0,
    use_count    INTEGER NOT NULL DEFAULT 0,
    keywords_json TEXT NOT NULL DEFAULT '[]',
    created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_heuristics_task_type ON heuristics_memory(task_type);

-- Prompt 演化版本
CREATE TABLE IF NOT EXISTS prompt_versions (
    id             TEXT    PRIMARY KEY,
    version        INTEGER NOT NULL,
    task_type      TEXT    NOT NULL,
    prompt_text    TEXT    NOT NULL,
    score          REAL    NOT NULL DEFAULT 0.0,
    cost           REAL    NOT NULL DEFAULT 0.0,
    source         TEXT    NOT NULL,
    parent_version INTEGER NOT NULL DEFAULT 0,
    is_active      INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_prompt_versions_active ON prompt_versions(task_type, is_active);

-- Staging 渐进发布状态
CREATE TABLE IF NOT EXISTS rollout_states (
    candidate_version TEXT    PRIMARY KEY,
    baseline_version  TEXT    NOT NULL,
    current_gate      INTEGER NOT NULL DEFAULT 0,
    canary_percent    INTEGER NOT NULL DEFAULT 0,
    status            TEXT    NOT NULL,
    started_at        INTEGER NOT NULL,
    last_advanced_at  INTEGER NOT NULL
);

-- Skill 描述符变体池（L2SkillGeneration 候选池）
CREATE TABLE IF NOT EXISTS skill_variant_pool (
    id              TEXT PRIMARY KEY,  -- UUIDv7
    skill_id        TEXT NOT NULL,     -- skills.name
    generation      INTEGER NOT NULL,  -- 0=原始, >0=突变/交叉
    fitness_score   REAL    DEFAULT 0.0,
    desc_variant    TEXT    NOT NULL,
    mutation_source TEXT    NOT NULL CHECK(mutation_source IN ('mutate', 'crossover', 'seed')),
    created_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_variant_skill    ON skill_variant_pool(skill_id, generation);
CREATE INDEX IF NOT EXISTS idx_variant_fitness  ON skill_variant_pool(skill_id, fitness_score);

-- Capability Gap 追踪记录（M4 挂起触发 -> M9 后台填补）
CREATE TABLE IF NOT EXISTS capability_gap_log (
    id                TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL,
    task_id           TEXT NOT NULL,
    required_tool     TEXT NOT NULL,
    description       TEXT NOT NULL,
    status            TEXT NOT NULL CHECK(status IN ('pending', 'searching', 'synthesizing', 'hitl_pending', 'resolved', 'failed')),
    trust_tier        INTEGER NOT NULL DEFAULT 1,
    fill_source       TEXT, -- 'marketplace' | 'synthetic'
    result_ref        TEXT, -- extension_instance id or skill id
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_cap_gap_status ON capability_gap_log(status);
