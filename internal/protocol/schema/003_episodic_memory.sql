-- ============================================================================
-- 003_episodic_memory: 情景记忆层 —— events 派生投影表
-- ============================================================================
-- 架构角色: 为 M5 HybridRetriever 提供优化的记忆检索入口。events 表（真相源）
--           为不可变日志，本表为其派生投影——增加检索优化字段，独立索引。
-- 生产者:    M2 OutboxWorker（异步投影）
-- 消费者:    M5 HybridRetriever / M4 ContextAssembler / M9 MEMF
-- 可变字段:  archived、decay_weight、salience（仅此 3 字段允许 UPDATE）
-- 写入路径:  M2 OutboxWorker 异步投影，禁止直接写 MutationBus
-- 关联:      M5(Memory) §3.1, M2(Storage) §2.1
-- ============================================================================

CREATE TABLE IF NOT EXISTS episodic_events (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id         TEXT    NOT NULL,
    seq                INTEGER NOT NULL,
    timestamp          INTEGER NOT NULL,
    event_type         TEXT    NOT NULL,  -- state_transition | tool_call | observation | reflection | system
    source             TEXT    NOT NULL,  -- agent | compaction | consolidation | persona_refinement
    content            TEXT    NOT NULL,
    embedding          BLOB,              -- float16 量化，4096 维，OutboxWorker 异步填充
    salience           REAL    NOT NULL DEFAULT 0.5,   -- 可 UPDATE，0.0-1.0
    decay_weight       REAL    NOT NULL DEFAULT 1.0,   -- 可 UPDATE，ForgettingManager 每日衰减
    occurred_at        INTEGER,
    embed_model_version TEXT   NOT NULL DEFAULT '',    -- 空字符串=未索引，OnlineReindexer 触发条件
    UNIQUE(session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_ep_time     ON episodic_events(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_ep_occurred ON episodic_events(occurred_at, session_id)
    WHERE occurred_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ep_embed_ver ON episodic_events(embed_model_version)
    WHERE embed_model_version = '';

-- ----------------------------------------------------------------------------
-- memory_group_mapping: 持续性记忆组映射（读时 LEFT JOIN，避免原位 UPDATE）
-- 生产者: M5 DurativeMemoryManager（每小时 cron）
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memory_group_mapping (
    event_id  INTEGER PRIMARY KEY,  -- episodic_events.id
    group_id  TEXT    NOT NULL,     -- DurativeGroup.ID (ULID)
    mapped_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mgm_group ON memory_group_mapping(group_id);
