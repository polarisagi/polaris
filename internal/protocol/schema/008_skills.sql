-- ============================================================================
-- 008_skills: Skill 运行时注册表（M6 SkillRegistry SSoT）
-- ============================================================================
-- 架构角色: 记录 Agent 可调用技能的执行元数据。SkillExecutor 唯一消费方。
--           目录/市场/安装记录见 extension_instances（020）。
-- 信任模型: trust_tier 替代已废弃的 signature_valid BOOLEAN（ADR-0016 §2.1）
--   TrustTier: 0=Untrusted, 1=Local, 2=Community, 3=Official, 4=System
-- 写入路径: 仅经 SkillRegistry.Register()，禁裸 SQL
-- 关联: M6(Skill Library), M7(Tool/Action Layer)
-- ============================================================================

CREATE TABLE IF NOT EXISTS skills (
    name        TEXT    PRIMARY KEY,             -- "skill:{slug}"，格式由 SkillRegistry 校验
    version     TEXT    NOT NULL,
    runtime     TEXT    NOT NULL DEFAULT 'script', -- 'script' | 'builtin'
    risk_level  TEXT    NOT NULL DEFAULT 'high', -- 'low' | 'medium' | 'high'
    sandbox     INTEGER NOT NULL DEFAULT 1,      -- 1=启用沙箱
    capabilities TEXT   NOT NULL,               -- JSON array
    exec_mode   TEXT    NOT NULL DEFAULT 'tool', -- 'tool' | 'ambient'
    ambient_priority TEXT NOT NULL DEFAULT 'auto', -- 'always' | 'auto' | 'index_only'
    trust_tier  INTEGER NOT NULL DEFAULT 0,      -- 0-4，见上方说明
    idempotent  INTEGER NOT NULL DEFAULT 0,      -- 1=幂等，允许缓存结果
    benchmarks  TEXT    NOT NULL DEFAULT '{}',   -- JSON: PassRate/AvgLatency 等
    instructions TEXT   NOT NULL DEFAULT '',
    deprecated  INTEGER NOT NULL DEFAULT 0,
    -- 来自插件 bundle 的技能标注其所属插件 ID（plugins.id = "pl_xxx"），
    -- 便于插件卸载时级联废弃；独立安装的技能留空。
    depends_on  TEXT    NOT NULL DEFAULT '[]',  -- JSON string[]，技能名列表；Register 时做环检测
    composes_of TEXT    NOT NULL DEFAULT '[]',  -- JSON string[]，此技能内聚合的子技能（超集）
    plugin_id   TEXT    NOT NULL DEFAULT '',
    needs_compat_check INTEGER NOT NULL DEFAULT 0, -- 1=需要沙箱集成测试验证兼容性
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_skills_deprecated  ON skills(deprecated);
CREATE INDEX IF NOT EXISTS idx_skills_risk_level  ON skills(risk_level);
CREATE INDEX IF NOT EXISTS idx_skills_trust_tier  ON skills(trust_tier);
CREATE INDEX IF NOT EXISTS idx_skills_plugin_id   ON skills(plugin_id);
CREATE INDEX IF NOT EXISTS idx_skills_ambient_priority ON skills(ambient_priority);
