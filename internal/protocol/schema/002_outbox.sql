-- ============================================================================
-- 002_outbox: 跨引擎最终一致投影 + 消费游标
-- ============================================================================
-- 架构角色: 跨存储引擎的异步投影中转站。以 EventLog 为真相源，将事件异步投影至
--           Pebble/sqlite-vec/SurrealDB 等目标引擎。嵌入式跨引擎 ACID 不可实现，
--           因此采用 Outbox + 幂等键保证最终一致性。
-- 生产者:    M2 DatabaseWriter（与业务写入同事务原子提交，
--           通过 CompositeMutationIntent 保证 events INSERT + outbox INSERT 原子）
-- 消费者:    M2 OutboxWorker（统一消费循环）、M5 Episodic 投影、M9 MEMF/Heuristics 投影、
--           M10 GraphRAG 投影
-- 不变量:
--   1. 版本高水位拦截: 目标引擎写入前校验 existing_version >= incoming_version
--      → 单消息幂等升级为版本偏序幂等
--   2. Poison Pill 毒丸驱逐: crash_recovery_count >= 3 → 直接标记 dead，
--      阻断确定性崩溃循环
--   3. 卡死 processing 恢复: Worker 启动时 UPDATE status='processing' → 'pending'；
--      Janitor 每 5 分钟恢复 processing 且 updated_at < now() - 300s 的记录
--   4. 已完成记录清理: status IN ('done','dead') 且 created_at < now() - 7d，
--      Janitor 每 6h 批量 DELETE (<=1000 行/批)
-- 写入路径: 仅通过 MutationBus CompositeMutationIntent [MutationBus]
-- 关联模块: M2(Storage Fabric) §2.5, M5(Memory) §3.1, M10(Knowledge RAG) §3.2
-- ============================================================================

CREATE TABLE IF NOT EXISTS outbox (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    -- ↑ 全局单调递增序号。Worker 基于 WHERE id > cursor 实现不重不漏增量消费。

    created_at      INTEGER NOT NULL,
    -- ↑ 创建时间（Unix 毫秒）。

    target_engine   TEXT NOT NULL,
    -- ↑ 目标引擎标识。'pebble' | 'sqlite_vec' | 'surrealdb' | 'tantivy' |
    --   'm10_graph_build' | 'm10_summary' | 'm10_compaction' | 'm10_cluster'。
    --   OutboxWorker 根据此字段路由到对应的 handler。

    operation       TEXT NOT NULL,
    -- ↑ 操作类型。'upsert' | 'delete'。

    scope           TEXT NOT NULL,
    -- ↑ 操作范围限定。如 'memory:episodic' | 'knowledge:doc_nodes'。

    payload         BLOB NOT NULL,
    -- ↑ Protobuf 序列化的投影数据。

    idempotency_key TEXT NOT NULL UNIQUE,
    -- ↑ 幂等键。格式: {target_engine}:{entity_type}:{entity_id}:{operation}:{version}。
    --   目标引擎写入前校验 existing_version >= incoming_version 实现版本偏序幂等。
    --   全局定义见 00-Global-Dictionary.md [Idempotency-Key]。

    status          TEXT NOT NULL DEFAULT 'pending',
    -- ↑ 生命周期: 'pending'(等待处理) → 'processing'(处理中) → 'done'(完成)
    --            | 'failed'(待重试，配合 next_retry_at 指数退避)
    --            | 'dead'(毒丸，attempts >= max 或 crash_recovery_count >= 3)。
    --   Worker 启动时将所有 'processing' 重置为 'pending'（崩溃恢复）。

    attempts        INTEGER NOT NULL DEFAULT 0,
    -- ↑ 已重试次数。每次处理失败后 +1，与 next_retry_at 配合实现指数退避重试。

    last_error      TEXT,
    -- ↑ 最近一次失败的错误信息。用于人工排查。

    next_retry_at   INTEGER,
    -- ↑ 下次重试时间（Unix 毫秒）。NULL 表示不需要重试或立即处理。

    crash_recovery_count INTEGER NOT NULL DEFAULT 0
    -- ↑ 连续崩溃计数。Worker 执行 FFI 前原子递增。
    --   >= 3 → 标记 status='dead'（毒丸驱逐），阻断确定性崩溃循环。
    --   注意: 此计数器在每次处理前递增，不由 Worker 重置——仅成功的处理会重置它。
);

-- 待重试失败记录（按 ID 排序，保证 FIFO）
CREATE INDEX IF NOT EXISTS idx_outbox_failed
    ON outbox(id) WHERE status = 'failed';

-- 待处理记录（按 ID 排序，保证 FIFO）
CREATE INDEX IF NOT EXISTS idx_outbox_pending
    ON outbox(id) WHERE status = 'pending';

-- ----------------------------------------------------------------------------
-- inbox_cursors: Worker 消费游标 —— 单调性防护
-- ----------------------------------------------------------------------------
-- 架构角色: 记录每个消费者的消费进度。Worker 在消费后同事务推进游标，
--          通过 WHERE excluded.last_seq_id > inbox_cursors.last_seq_id 保证单调性。
-- 生产者:    M2 OutboxWorker（消费后更新）
-- 消费者:    M2 OutboxWorker（启动时读取恢复消费位置）
-- ============================================================================

CREATE TABLE IF NOT EXISTS inbox_cursors (
    consumer_id   TEXT PRIMARY KEY,
    -- ↑ 消费者标识。如 'outbox_worker_main' | 'outbox_worker_pebble'。

    last_seq_id   INTEGER NOT NULL,
    -- ↑ 已消费的最后一条 outbox.id。重启时从此位置 + 1 继续消费。

    updated_at    INTEGER NOT NULL
    -- ↑ 最后更新时间（Unix 毫秒）。
);

-- 原子推进游标: 强制单调性——新 seq_id 必须大于旧 seq_id
-- INSERT INTO inbox_cursors (consumer_id, last_seq_id, updated_at) VALUES (?, ?, ?)
-- ON CONFLICT(consumer_id) DO UPDATE SET
--     last_seq_id = excluded.last_seq_id,
--     updated_at = excluded.updated_at
-- WHERE excluded.last_seq_id > inbox_cursors.last_seq_id;
