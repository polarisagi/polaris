-- ============================================================================
-- 004_semantic_memory: 语义记忆层 —— 实体/关系图 + 连通性缓存
-- ============================================================================
-- 架构角色: 存储从情景记忆中提取的结构化知识（实体、关系、事实）。
--           Tier 0: SQLite 邻接表（vertice=edge 模式）；Tier 1+: SurrealDB 图存储。
-- 生产者:    M5 Consolidation Pipeline（Stage 2: Upsert Semantic —— 事件提取事实后写入）
-- 消费者:    M5 HybridRetriever（图遍历检索路径）、M10 Knowledge-RAG（跨文档实体链接）、
--           M4 Agent Kernel（事实检索）
-- 不变量:
--   1. version++ 不可变版本 + source_event_id 溯源（每条事实可追溯到其来源事件）
--   2. 信念修正: 矛盾事实优先保留更近期/更高证据强度的一方
--   3. Prospective Indexing: 写入时预生成未来可能的查询索引
--   4. semantic_connectivity_cache 为派生数据缓存（冷后台预计算），非事实源
-- Tier 0: SQLite 邻接表; Tier 1+: SurrealDB 图存储（Tier 0 不加载 SurrealDB）
-- 写入路径: M2 OutboxWorker 异步投影（冷路径，不阻塞 Agent 回复）
-- 关联模块: M5(Memory) §4.1, §8, M10(Knowledge RAG) §2.6, M4(Agent) §7
-- ============================================================================

CREATE TABLE IF NOT EXISTS semantic_entities (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    -- ↑ 实体自增 ID，内部引用用。跨系统引用使用 name + entity_type 联合标识。

    entity_type     TEXT NOT NULL,
    -- ↑ 实体类型。'Person' | 'Project' | 'Tool' | 'Concept' | 'Document' | 'API' |
    --   'ConfigParam' | 'BusinessRule' | 'DataType'。
    --   M5 GraphTraverser BFS 遍历时使用 entity_type 加权: Person×2.0, Project×1.5, Tool×1.0。

    name            TEXT NOT NULL,
    -- ↑ 实体名称。与 entity_type 联合唯一。

    properties      JSON,
    -- ↑ 实体属性（JSON）。如 {"language": "Go", "version": "1.26"}。

    embedding       BLOB,
    -- ↑ 实体向量表示（float16 量化，4096 维）。用于 M5 HybridRetriever 的种子实体检索。

    version         INTEGER DEFAULT 1,
    -- ↑ 不可变版本号。每次 UPDATE 递增 1。M5 UpsertFact 基于此做信念修正。

    created_at      INTEGER NOT NULL,
    -- ↑ 创建时间（Unix 毫秒）。

    updated_at      INTEGER NOT NULL,
    -- ↑ 最后更新时间（Unix 毫秒）。

    source_event_id INTEGER,
    -- ↑ 溯源: 指向产生此实体的 M2 events.id。M11 AuditTrail 基于此追溯事实来源。

    status          TEXT NOT NULL DEFAULT 'active',
    -- ↑ 生命周期状态: 'active' | 'superseded' | 'expired' | 'merged'。
    --   信念修正: 同类型近似实体写入时将旧版本标记为 'superseded'。
    --   HybridRetriever 扫描时加 WHERE status='active' 过滤，消除旧矛盾事实污染。
    --   来源: supermemory temporal belief revision + PruneMem lifecycle governance。

    superseded_by   INTEGER REFERENCES semantic_entities(id),
    -- ↑ 被取代者指向新实体的 DB id（status='superseded' 时填充）。
    --   提供完整溯源链: 当前活跃版本 ← 历史版本序列（可回溯）。

    UNIQUE(entity_type, name)
    -- ↑ 实体类型 + 名称唯一 —— 同一实体多次提取通过 UpsertFact 更新属性。
);

CREATE TABLE IF NOT EXISTS semantic_relations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    -- ↑ 关系自增 ID。

    source_id       INTEGER REFERENCES semantic_entities(id),
    -- ↑ 关系起点实体 ID。

    target_id       INTEGER REFERENCES semantic_entities(id),
    -- ↑ 关系终点实体 ID。

    relation_type   TEXT NOT NULL,
    -- ↑ 关系类型。'USES' | 'DEPENDS_ON' | 'IMPLEMENTS' | 'CONTAINS' | 'RELATED_TO' |
    --   'BEFORE' | 'AFTER' | 'USER_PREFERENCE'。

    weight          REAL DEFAULT 1.0,
    -- ↑ 关系权重 0.0-1.0。M5 Spreading Activation 扩散激活时作为传播系数。
    --   M5 SynapticPlasticityManager 基于 LTP（强化）/ LTD（衰减）动态调整。

    properties      JSON,
    -- ↑ 关系属性（JSON）。如 {"confidence": 0.9, "evidence_count": 5}。

    created_at      INTEGER NOT NULL,
    -- ↑ 创建时间（Unix 毫秒）。

    source_event_id INTEGER,
    -- ↑ 溯源: 指向产生此关系的 M2 events.id。

    UNIQUE(source_id, target_id, relation_type)
    -- ↑ 同一对实体间同一关系类型唯一 —— 重复提取时 UPDATE weight/updated_at。
);

-- 出边索引（source → target，BFS 正向遍历）
CREATE INDEX IF NOT EXISTS idx_semantic_rel_source ON semantic_relations(source_id);
-- 入边反向索引（target → source，BFS 反向遍历 + 双向路径检索）
CREATE INDEX IF NOT EXISTS idx_semantic_rel_target ON semantic_relations(target_id);

-- 生命周期索引（status 查询加速，HybridRetriever WHERE status='active'）
CREATE INDEX IF NOT EXISTS idx_semantic_ent_status ON semantic_entities(status);
-- 被取代链索引（superseded_by 溯源查询）
CREATE INDEX IF NOT EXISTS idx_semantic_ent_superseded ON semantic_entities(superseded_by) WHERE superseded_by IS NOT NULL;

-- ----------------------------------------------------------------------------
-- semantic_connectivity_cache: Effective Connectivity 派生缓存
-- ----------------------------------------------------------------------------
-- 架构角色: 冷后台预计算实体间的高效连通路径（connectivity precomputation）。
--           查询时 O(1) 查表，避免实时 BFS/Spreading Activation 全图遍历。
-- 注意: 此为派生数据缓存，非事实源。每日凌晨 4:30 cron 全量覆盖计算。
-- Tier 门控: Tier 0 → 最多计算 200 个种子实体 (~20MB)；Tier 1+ → 1000 个。
-- 生产者:    M5 ConnectivityPrecomputer（冷后台）
-- 消费者:    M5 ActivationMaximization（O(1) 查表获取最高效激活路径）
-- ============================================================================

CREATE TABLE IF NOT EXISTS semantic_connectivity_cache (
    source_id        TEXT NOT NULL,
    -- ↑ 起点实体 ID。

    target_id        TEXT NOT NULL,
    -- ↑ 终点实体 ID。

    effective_weight REAL,
    -- ↑ 高效连通权重（LTP 强化后 × 活跃度衰减后）。

    hop_distance     INTEGER,
    -- ↑ 跳数。1 = 直接关系，2+ = 间接路径。

    computed_at      INTEGER NOT NULL,
    -- ↑ 计算时间（Unix 毫秒）。

    updated_at       INTEGER NOT NULL,
    -- ↑ 最后更新时间。

    PRIMARY KEY (source_id, target_id, hop_distance)
);

-- ============================================================================
-- user_profile: 用户画像（L3 Persona 等价物）
-- ============================================================================
-- 架构角色: 从 Episodic 事件中自动合成的用户画像，每 50 条新事件触发一次合成。
--           替代 ImmutableCore.UserPreferences 的静态 map，可随会话自动演化。
-- 生产者: M5 ConsolidationPipeline Stage 3.5（UserProfileSynthesizer）
-- 消费者: M4 Agent Kernel（VolatileBlock 注入）、M5 HybridRetriever（语义检索增强）
-- 来源: supermemory User Profile + TencentDB L3 Persona 收敛方案。
-- ============================================================================

CREATE TABLE IF NOT EXISTS user_profile (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,

    profile_key         TEXT NOT NULL UNIQUE DEFAULT 'default',
    -- ↑ 画像键。单 Agent 单用户固定 'default'；多用户场景用 user_id。

    stable_facts        JSON,
    -- ↑ 稳定事实: 用户角色/技能/语言偏好等低频变化信息。
    --   例: {"role":"data_scientist","lang":"zh-CN","timezone":"Asia/Shanghai"}

    recent_activity     JSON,
    -- ↑ 近期行为摘要（最近 7d，最多 20 条），滚动更新，旧条目按 TTL 淘汰。

    behavioral_patterns JSON,
    -- ↑ 行为模式: 常用工具频率、编码风格偏好、沟通习惯。
    --   由 LLM 从 stable_facts + recent_activity 归纳（provider=nil 时规则 fallback）。

    synthesis_count     INTEGER NOT NULL DEFAULT 0,
    -- ↑ 累计合成次数，用于调试与触发频率控制。

    last_event_ts       INTEGER,
    -- ↑ 最后一次合成时最新事件的 Unix 毫秒时间戳，避免重复处理已消费事件。

    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);
