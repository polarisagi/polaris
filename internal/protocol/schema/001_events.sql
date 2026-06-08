-- ============================================================================
-- 001_events: 系统唯一真相源 EventLog
-- ============================================================================
-- 架构角色: 全系统所有状态变更的不可变日志。所有模块的状态变更必须写入此表。
--           M5 episodic_events 为此表的派生投影表（记忆检索优化），通过 idempotency_key 关联。
-- 生产者:    M2 EventWriteBuffer → DatabaseWriter（统一单写者串行化）
-- 消费者:    M4 Agent Kernel（崩溃恢复回放）、M5 Episodic Memory（派生投影）、
--           M8 Blackboard（初始化重建）、M11 AuditTrail（hash chain 完整性验证）
-- 不变量:
--   1. offset AUTOINCREMENT 全局全序，物理不可回退（NTP 漂移/时钟回退免疫）
--   2. id ULID 时间有序（UUID v4 破坏时间局部性，不采用）
--   3. payload Protobuf 序列化（Schema 演化，Go↔Rust 同 .proto 生成）
--   4. 本表为不可变真相源 —— 仅 INSERT，绝不 UPDATE。派生表可变字段见 003_episodic_memory
--   5. 所有状态可从 events + outbox 重建，满足 [HE-Rule-6] State-in-DB
--   6. M11 hash chain 覆盖本表全字段
-- 写入路径: 仅通过 MutationBus DatabaseWriter 单写者（禁止业务 goroutine 直写 SQLite）
-- 关联模块: M2(Storage Fabric) §2.1, M4(Agent Kernel) §9, M5(Memory) §3.1, M11(Policy) §7
-- ============================================================================

CREATE TABLE IF NOT EXISTS events (
    offset            INTEGER PRIMARY KEY AUTOINCREMENT,
    -- ↑ 全局单调递增物理序号。NTP 漂移/时钟回退免疫。AUTOINCREMENT 保证永不回收已用序号。
    --   消费者通过 WHERE offset > cursor 实现不重不漏的增量消费。

    id                TEXT NOT NULL UNIQUE,
    -- ↑ 事件逻辑标识，ULID 格式。时间有序（UUID v4 破坏时间局部性，不采用）。
    --   跨系统引用时使用此 ID 而非 offset（offset 在重建时可能变化）。

    topic             TEXT NOT NULL,
    -- ↑ 事件主题/命名空间。如 "agent.task"、"memory.consolidation"、"policy.deny"。
    --   与 M8 Blackboard.TaskType 对齐，用于按领域分区消费。

    actor             TEXT NOT NULL,
    -- ↑ 事件触发者。格式: "agent:{agent_id}" | "system:{module_name}" | "user:{session_key}"。
    --   M11 AuditTrail 基于此字段做不可否认性追踪。

    type              TEXT NOT NULL,
    -- ↑ 事件类型枚举。state_transition | tool_call | observation | reflection | system。
    --   对应 M4 FSM 中的状态转移触发类型。

    payload           BLOB NOT NULL,
    -- ↑ Protobuf 序列化的事件体。Go↔Rust 同 .proto 生成，保证跨语言序列化一致。
    --   禁止 JSON 裸存 —— 无 Schema 约束，破坏类型安全 [HE-Rule-3]。

    idempotency_key   TEXT UNIQUE,
    -- ↑ 幂等键。格式: {target_engine}:{entity_type}:{entity_id}:{operation}:{version}。
    --   Outbox Worker 基于此键保证至少一次投递的幂等消费。
    --   全局定义见 00-Global-Dictionary.md [Idempotency-Key]。

    embedding         BLOB,
    -- ↑ 事件向量表示（float16 量化，4096 维）。M5 Consolidation 阶段异步填充。
    --   用于 M5 HybridRetriever 的 Dense Vector 语义检索路径。

    memory_layer      TEXT DEFAULT 'episodic',
    -- ↑ 事件所属记忆层: 'episodic'(情景) | 'semantic'(语义) | 'procedural'(程序)。
    --   默认值为 episodic，M5 Consolidation 提取后可能升级为 semantic。

    salience          REAL DEFAULT 0.5,
    -- ↑ 事件显著性 0.0-1.0。初始值 0.5 表示未评估。
    --   生产者: M4 LLM 输出评估 + M5 Consolidation 计算。
    --   消费者: M9 MEMF 剪枝过滤 + M4 ContextAssembler 上下文排序（高显著性事件优先注入 prompt）。

    occurred_at       INTEGER,
    -- ↑ 事件发生的物理时间（Unix 毫秒）。不同于 created_at（写入时间），
    --   用于 M5 DurativeMemory 的时间邻近度聚类。

    durative_group_id TEXT,
    -- ↑ M5 DurativeMemory 持续性记忆组 ID。事件按语义连续性分组后回填此字段。
    --   读时 LEFT JOIN memory_group_mapping 合成 durative_group_id，避免原位 UPDATE events。

    created_at        INTEGER NOT NULL,
    -- ↑ 写入时间（Unix 毫秒），DatabaseWriter COMMIT 时设置。

    metadata          TEXT,
    -- ↑ 扩展元数据（JSON），如 trace_id、session_id。非结构化补充字段。
    --   4KB 硬限 —— 超出内容须垂直拆分至 VFS，热表仅存 vfs_ref 指针 [HE-Rule-6]。

    prev_hash         TEXT,
    -- ↑ 前一事件的 hash 值（M11 hash chain 链首为 NULL）。

    hash              TEXT
    -- ↑ SHA-256(id || topic || actor || type || payload || prev_hash)，hex 编码。
    --   M11 AuditTrail 通过重算 hash 链验证 EventLog 不可否认性。
);

-- 按 topic + offset 分区消费（Outbox Worker 增量拉取的主索引）
CREATE INDEX IF NOT EXISTS idx_events_topic_offset
    ON events(topic, offset);

-- 按 actor 审计追踪（M11 AuditTrail 按操作者查询）
CREATE INDEX IF NOT EXISTS idx_events_actor
    ON events(actor, offset);

-- 按 memory_layer 分区检索（M5 HybridRetriever 按记忆层过滤）
CREATE INDEX IF NOT EXISTS idx_events_memory_layer
    ON events(memory_layer, offset) WHERE memory_layer IS NOT NULL;

-- 幂等键快速去重（Outbox Worker 消费前幂等检查）
CREATE INDEX IF NOT EXISTS idx_events_idempotency
    ON events(idempotency_key) WHERE idempotency_key IS NOT NULL;

-- 按物理发生时间时序扫描（M5 DurativeMemory 时间邻近度聚类）
CREATE INDEX IF NOT EXISTS idx_events_occurred
    ON events(occurred_at, offset) WHERE occurred_at IS NOT NULL;

-- 按持续性记忆组查询（M5 RetrieveWithDurative 读时合成）
CREATE INDEX IF NOT EXISTS idx_events_durative
    ON events(durative_group_id, occurred_at) WHERE durative_group_id IS NOT NULL;
