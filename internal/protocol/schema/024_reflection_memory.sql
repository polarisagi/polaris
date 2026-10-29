-- ============================================================================
-- 024_reflection_memory: 元认知反思层 — 替代 KV 前缀存储
-- ============================================================================
-- 架构角色: M5 §3.4 ReflectionMemory（Mem-L1.5），Agent 跨 Session 经验积累。
-- 写入路径: ReflectionWorker.ConsolidateReflections → MutationBus → DatabaseWriter
-- 读取路径: M4 S_PERCEIVE + S_PLAN 按 task_type 拉取 top-3，注入上下文。
--           HybridRetriever 第 4 路（权重 0.15）BM25 扫描。
-- 容量约束: 硬上限 5000 条（HT0），LRU 淘汰最久未访问（last_accessed_at）
-- 迁移兼容: 原 KV 前缀 "reflection:{id}" 数据由 GC 在首次启动后自动清理（尽力而为）
-- ============================================================================

CREATE TABLE IF NOT EXISTS reflection_memory (
    id                TEXT    PRIMARY KEY,       -- "ref_{UnixNano}"
    session_id        TEXT    NOT NULL,           -- 触发该反思的 Session ID
    agent_id          TEXT    NOT NULL DEFAULT '',
    task_type         TEXT    NOT NULL DEFAULT '', -- 任务类型，S_PERCEIVE 按此检索
    reflection_type   TEXT    NOT NULL DEFAULT '', -- success_pattern|failure_mode|efficiency_insight|cross_task_principle
    content           TEXT    NOT NULL,           -- 反思正文（≤500 tokens）
    fail_reason       TEXT    NOT NULL DEFAULT '',
    strategy          TEXT    NOT NULL DEFAULT '', -- 策略切换描述（含于 Topic 检索）
    decision          TEXT    NOT NULL DEFAULT '', -- 元决策内容（含于 Topic 检索）
    salience          REAL    NOT NULL DEFAULT 0.8,
    embedding         BLOB,                       -- float16 量化，OutboxWorker 异步填充
    embed_model_ver   TEXT    NOT NULL DEFAULT '',
    accessed_count    INTEGER NOT NULL DEFAULT 0,
    last_accessed_at  INTEGER NOT NULL DEFAULT 0,
    evidence_ids_json TEXT    NOT NULL DEFAULT '[]', -- EvidenceEventIDs JSON 数组
    meta_json         TEXT    NOT NULL DEFAULT '{}',
    created_at        INTEGER NOT NULL
) STRICT;

-- 核心检索索引：task_type 过滤 + created_at 时间降序
CREATE INDEX IF NOT EXISTS idx_reflect_task_type  ON reflection_memory(task_type, created_at DESC);
-- LRU 淘汰索引
CREATE INDEX IF NOT EXISTS idx_reflect_lru        ON reflection_memory(last_accessed_at ASC);
-- 全文检索辅助索引（salience 高优先）
CREATE INDEX IF NOT EXISTS idx_reflect_salience   ON reflection_memory(salience DESC);
CREATE INDEX IF NOT EXISTS idx_reflect_session    ON reflection_memory(session_id);
