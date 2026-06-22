-- ============================================================================
-- 003_episodic_memory: 情景记忆层 —— events 派生投影表
-- ============================================================================
-- 架构角色: 为 M5 HybridRetriever 提供优化的记忆检索入口。events 表（真相源）
--           为不可变日志，本表为其派生投影——增加检索优化字段，独立索引。
-- 生产者:    M2 OutboxWorker（异步投影）
-- 消费者:    M5 HybridRetriever / M4 ContextAssembler / M9 MEMF
-- 可变字段:  archived、archive_offset、decay_weight、salience（仅此 4 字段允许 UPDATE）
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
    event_uuid         TEXT    NOT NULL DEFAULT '',    -- 原始 Event.ID（UUID）；OutboxWorker 填充，供 SurrealDB VecUpsert/FTSIndex 使用
    archived           INTEGER NOT NULL DEFAULT 0,     -- 逻辑归档标志：0=active, 1=archived（GDPR Art.17 合规删除替代）
    archive_offset     INTEGER,                        -- 归档时的事件序号偏移（冷归档窗口边界）
    reasoning_state    TEXT,                           -- CoT 轨迹 -- encrypted: AES-256-GCM

    information_value REAL NOT NULL DEFAULT 0.5,
    -- ↑ 写入前主动价值评估得分（WriteFilter）。
    --   0.0-1.0，阈值 0.4。WriteFilter 跳过的条目不写入 semantic_entities。
    --   LLM 评估（DeepSeek V4 优先），provider=nil 时走启发式 fallback。

    UNIQUE(session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_ep_time     ON episodic_events(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_ep_occurred ON episodic_events(occurred_at, session_id)
    WHERE occurred_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ep_active ON episodic_events(session_id, timestamp)
    WHERE archived = 0;
CREATE INDEX IF NOT EXISTS idx_ep_uuid ON episodic_events(event_uuid)
    WHERE event_uuid != '';

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

-- ----------------------------------------------------------------------------
-- episodic_events_change_log: 事件冷冻操作审计日志
-- 生产者: M5 EpisodicMem.MarkCold
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS episodic_events_change_log (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id     TEXT    NOT NULL,
    changed_at     INTEGER NOT NULL,
    change_type    TEXT    NOT NULL,
    affected_count INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ep_change_log_session ON episodic_events_change_log(session_id, changed_at);
