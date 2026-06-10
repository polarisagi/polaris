# 模块 5: Memory System

> 四层记忆（Working / Episodic / Semantic / Procedural），多存储引擎绑定，[Tier-0-Limit]
> Go（记忆管理器 + 检索路由 + Consolidation），Rust（Embedding 计算 via M1）
> [HE-Rule-4] [HE-Rule-5] [HE-Rule-6]
> **§跳读**: 0-bis:7 职责 / 0-ter:19 不变量速查 / 1:30 四层映射 / 2:39 L0 Working / 3:116 L1 Episodic / 4:214 L2 Semantic / 5:254 L3 Procedural / 6:264 写路径 / 7:276 HybridRetriever / 8:377 EffConn / 9:387 Consolidation / 10:408 Forgetting / 11:425 ContextAssembler / 12:495 Drift / 14:533 496(SOFT)降级 / 15:555 依赖
## 0-bis. 职责边界

- M5 **是**: 四层记忆（Working/Episodic/Semantic/Procedural）的读写管理器 | M5 **不是**: 记忆的物理存储引擎（那是 M2）
- M5 **是**: HybridRetriever 检索路由（BM25 + Dense + Graph → RRF） | M5 **不是**: Embedding 向量计算（那是 M1 Embedding API）
- M5 **是**: Consolidation 语义压缩（episodic→semantic） | M5 **不是**: Skill 技能执行（那是 M6）
- M5 **是**: ImmutableCore 用户长期偏好管理（永不裁剪）+ UserProfile 自动合成（L3 Persona，§9.5） | M5 **不是**: 安全策略决策（那是 M11）
- M5 **是**: ContextAssembler 上下文组装（Slot 分离 + Taint 门控）+ TaskMermaidCanvas 符号化压缩（§11.3） | M5 **不是**: Agent 任务调度（那是 M4）
- M5 **是**: Forgetting 策略（效用衰减 + 冷归档） | M5 **不是**: 物理数据删除（委托 M2 Storage）
- M5 **是**: Entity 生命周期治理（active/superseded/expired）+ Jaccard 矛盾检测（§4.2）| M5 **不是**: 策略执行（那是 M11）

---

## 0-ter. 不变量速查表

- 编号: inv_M5_01 | 不变量: ImmutableCore 永不参与压缩——ContextWindow.Compress 跳过此区域 | 验证方式: CI `immutable_core_integrity` 测试
- 编号: inv_M5_02 | 不变量: ImmutableCore 写入必经 staging 审批——`provenance_id` CHECK 约束 | 验证方式: DDL CHECK + M11 审计
- 编号: inv_M5_03 | 不变量: embed_model_version 是一等字段——每 chunk/event 携带，跨版本检索走 OnlineReindexer | 验证方式: DDL NOT NULL 约束
- 编号: inv_M5_04 | 不变量: 默认归档不物理删除——`archived=1` 级联 chunks/entities，GDPR Art.17 例外 | 验证方式: M2 Forgetting 审计
- 编号: inv_M5_05 | 不变量: RRF 融合不裸加权——`weight/(k+rank+1)`，k 见 `spec/state.yaml §m5_memory.rrf_k`，防止不同检索器分数尺度不可比 | 验证方式: HybridRetriever 代码审计
- 编号: inv_M5_06 | 不变量: Taint 传播——chunks/events/RetrievedItem 均携带 5 级 TaintLevel，只升不降 | 验证方式: CI `taint_propagation` 测试

---

## 1. 四层记忆物理映射

- 记忆层: L0 Working | 物理存储: 进程内 theine-go cache + Immutable Core(永不裁剪) + NotesStore(SQLite 持久化) | 读写比: 1:1000 | 延迟要求: <1µs
- 记忆层: L1 Episodic | 物理存储: [Storage-SQLite] session_events + [Storage-SurrealDB-Core] embedding 列 | 读写比: 100:1 | 延迟要求: 写<100µs, 读<5ms
- 记忆层: L2 Semantic | 物理存储: [Storage-SurrealDB-Core] | 读写比: 1:50 | 延迟要求: <10ms
- 记忆层: L3 Procedural | 物理存储: [Storage-SurrealDB-Core] skill_id→blob + [Storage-SurrealDB-Core] 语义检索 + 文件系统 SKILL.md | 读写比: 1:500 | 延迟要求: <10µs / <5ms

---

## 2. L0 Working Memory

### 2.1 核心结构

WorkingMemory/ImmutableCore/ActiveContext/Task/Observation/MemoryFragment 类型定义见 `pkg/cognition/memory/memory.go` 和 `pkg/cognition/memory/working_mem.go`。NotesStore 见 `pkg/cognition/memory/notes_store.go`；UserProfile 见 `pkg/swarm/persona_refiner.go`。

**写入权分离**: M11 Policy → ImmutableCore.SafetyConstraints; M9 PersonalizationWorker → ImmutableCore.UserPreferences + InteractionSummary; 用户显式 `/set` → UserPreferences; M5 Memory System → 仅读取，永不写入。

**ContextZone**:
```
ZoneImmutable=0      // 存放系统全局目标、Agent 身份认知(包含底层模型及工具列表)以及用户长期偏好和安全约束，置于最顶部且永不被裁剪
ZoneMutableSkill=1   // SKILL.md 描述模板，M9 PromptOptimizer 合法优化靶点
ZoneTaintedData=2    // 外部数据，[TaintLevel] Tracked，永不进入指令区
```

**M9 → ZoneMutableSkill Taint Gate（双层）**:
1. **自动层**: PromptOptimizer 输出 → M11 `SanitizeBySchema` + `SanitizeByDeterministicTransform` → SIC 检测 → 阳性丢弃 + 审计 `prompt_opt_taint_rejected`
2. **HITL 层**: 前 5 次写入经 LLM-as-Judge 语义审核 → unsafe → [ESCALATE]。累计 5 次通过 → 概率抽查 (20%/次，安全随机源)，优先抽查"语义距离 >2σ"输出（历史 PromptOptimizer embedding 分布）。命中 + unsafe → 撤销该 task_type 全部豁免 + 语义漂移分析（余弦距离 >2σ → CRITICAL）。禁止永久豁免
3. **独立 LLM-as-Judge 二次审查**: 输出合并前必经独立 Judge（不同 Provider 模型）审查。不通过 → 丢弃 + HITL。防 SIC 对间接指令注入的 false negative

**ContextAssembler.Append**:
0. **InteractionSummary 特例**: M9 PersonaRefiner 生成的 InteractionSummary (`source='persona_refinement'` + Ed25519 签名) 写 ZoneImmutable 前执行 `SanitizeByDeterministicTransform`（保留 <200 tokens 摘要 + SHA-256 校验和），TaintLevel 强制 TaintLow。固定白名单——仅 `source='persona_refinement'` + 有效 M9 签名可写 ZoneImmutable
1. zone==ZoneImmutable 且内容 TaintLevel > TaintLow → panic 拒绝 (tainted 数据越界进不可变区)
2. zone==ZoneTaintedData 且内容 Tainted → 接受写入（正确归宿）; zone!=ZoneTaintedData 且 TaintLevel >= TaintMedium → 降级路由到 ZoneTaintedData + WARN + 审计事件 `context_assembler_taint_zone_routing`
3. zone==ZoneMutableSkill → 验证 Ed25519 ApprovalSignature（M9 签发）→ 签名无效则降级 ZoneTaintedData + WARN + 审计事件 `context_assembler_mutable_skill_integrity_failed`
4. 签名通过 → Monotonic Version Gate: 查询 `sys_config.min_skill_version`，version < min → 拒绝 + CRITICAL + 审计事件 `context_assembler_rollback_attack_blocked`
5. 写入对应 zone string builder

**ContextAssembler.Build**: 固定顺序 ZoneImmutable → ZoneMutableSkill → ZoneTaintedData。ZoneTaintedData 追加前，对 `[TaintLevel] >= TaintMedium` 的内容执行 M11 Spotlighting 包裹：
  `=== UNTRUSTED_DATA_{sha256(content)[:8]} ===\n{content}\n=== END_UNTRUSTED_DATA ===`
spotlight_hex 由内容 SHA-256 前 8 位派生（非随机）→ 同内容同标记，PromptFn 保纯函数性，M12 Eval 回放可验证。M4 上下文注入同此规则。

**SessionResume（崩溃后 ActiveContext 重建）**:
1. 从 M4 FSM Snapshot 恢复 FSM 状态 + DAG 进度
2. 从 [Storage-SQLite] `session_events` 按 AUTOINCREMENT 读取 Snapshot 后 Episodic Events
3. 加载 NotesStore 中关联活跃 Note（未过期/未删除）
4. 以 Snapshot WorkingMemorySummary 为基底，重放 Episodic Events → 重建 ActiveContext
5. ActiveContext 就绪 → Agent FSM 恢复执行
约束: <500ms 完成（Snapshot 后事件 <1000 条）。Snapshot 损坏 → 全量重建。Notes 懒加载（仅 5 条热 Note）。>1000 条事件按 200 条/批增量

### 2.2 NotesStore

工作记忆的持久化存储，为 Agent 提供跨 Session 的轻量笔记能力。实现见 `pkg/cognition/memory/notes_store.go`（`SQLNotesStore` / `InMemNotesStore`）。DDL 见 `internal/protocol/schema/023_notes.sql`。

**存储约束**: 单条 Note 上限 64KB（`content`），默认 TTL 为 7 天（`expires_at = now + 7d`）。支持基于系统保留标签（如 `task:{taskID}`）进行任务级别的笔记聚合检索（`ListByTask`）。

**写路径**: 直接 SQL 写（`ON CONFLICT DO UPDATE`，CAS `version++`），与 `PromptVersionStore` 同级别。CAS 失败时静默幂等（调用方可重试）。注：原规范要求通过 MutationBus；实际设计中 NotesStore 为 L0 单 Agent 私有状态，不跨 Agent 共享，无需通过 MutationBus 保证跨 Agent 顺序性。

**容量管理**: 过期 Note 由 `GC()` 定期清理（直接 DELETE，调用方负责调度）。

### 2.3 用户画像

实现见 `pkg/swarm/persona_refiner.go`（`PersonaRefiner` + `UserProfile`）。持久化到 `preferences` 表（key=`"user_profile"`，单条 JSON）。

**设计决策（简化）**: 原规范的 11 维度对自托管单用户场景过度设计。简化为 5 个实用维度，删除 ColdStartManager 和 ProactiveQuery（无生产价值证据）。

**UserProfile 5 维度**:

| 维度 | 含义 | 取值示例 |
|------|------|---------|
| `language_pref` | 首选回复语言 | `zh-CN` / `en` / `mixed` |
| `response_style` | 回复风格 | `concise` / `detailed` / `casual` / `formal` |
| `output_format` | 输出格式 | `markdown` / `plain` / `code-first` |
| `expertise` | 领域专业程度（影响解释深度） | `novice` / `intermediate` / `expert` |
| `interaction_summary` | LLM 在 Session 结束时生成的用户特征摘要（≤200 字） | 文字描述 |

**PersonaRefiner（会话结束 hook）**:
- `Load(ctx)`: 启动时从 `preferences` 表加载画像；不存在则用默认值（冷启动）
- `Update(signals map[string]string)`: 接收显式信号（`/set style concise` 等）即时更新
- `RefineAtSessionEnd(ctx, msgs)`: 会话结束时调用 LLM 更新 `interaction_summary`（provider 为 nil 时跳过）
- `Save(ctx)`: 持久化当前画像；与 `RefineAtSessionEnd` 配对调用
- `ToUserPreferences()`: 转为 `[]protocol.UserPreference`，注入 `ImmutableCore.UserPreferences`

**与 M9 PersonaRefiner 区别**: PersonaRefiner 更新**用户偏好画像**；ReflectionMemory 更新 **Agent 自身执行经验**。

---

## 3. L1 Episodic Memory

### 3.1 episodic_events 表

> `episodic_events` 是 M2 `events` 表（[EventLog] 真相源）的派生投影表。Agent 动作写入 M2 `events` → Outbox Worker 异步提取事件至 `episodic_events` 并填充 embedding/salience/decay_weight。

**可变字段白名单**: `episodic_events` 为派生投影表，允许受控字段变更（`archived`, `decay_weight`, `salience`, `archive_offset`），其余字段 append-only。每次受控字段变更须写入 `episodic_events_change_log` 表（再由该 log 表参与 M11 hash chain）。M2 `events` 表（真相源）仅 INSERT，绝不 UPDATE。M11 hash chain 覆盖 `events` 表全字段 + `episodic_events_change_log` 表。

**`[ReasoningState]` 列**（M4 §7.1 跨轮持久化）: `episodic_events.reasoning_state BLOB(nullable)` — Provider 返回的推理状态 blob，msgpack + AES-256-GCM 加密（key 由 [CredentialVault].persistent_key 派生）。同 task_id 30min 窗口内 M4 可读取最近一条注入下次 LLM 调用。SessionPIIVault.SecureZero 同步清零本字段。Tier 0 默认不写（关闭 `FeatureReasoningStateCarry`）。

DDL 和持续性记忆组映射表见 `internal/protocol/schema/003_episodic_memory.sql`。

Salience: LLM 输出 + 工具结果 → 低; 用户反馈 + 关键决策 + 失败/成功 → 高。基于 M9 SurpriseIndex 信号的边权重强化与时间衰减实现动态权重调整（见 §7.6）。

**EpisodicGraphIndexer（GraphRAG 融合）**:
`EpisodicMem.Append()` 写 SQLite 后，同步调用 `EpisodicGraphIndexer.Index(ev)` 在 SurrealDB 图谱中建立边：
- `episodic:{id} → TRIGGERED_BY → agent:{agentID}`（事件来源追溯）
- `episodic:{id} → IN_SESSION  → session:{taskID}`（会话聚类检索）
- `episodic:{id} → ACTION_DONE → entity:tool:unknown`（工具调用关联，`EventActionDone` 类型）

实现见 `pkg/cognition/memory/episodic_graph_bridge.go`。边写入为 best-effort（失败仅记日志，不阻断写路径）。通过 `NewMemImplWithGraph(store, graph)` / `NewMemorySystemWithGraph(store, graph)` 注入图存储；indexer=nil 时跳过图桥。

此设计打通 episodic↔knowledge 两个孤岛：HybridRetriever 图遍历路径（§7.3）从知识实体出发可跨越到 episodic 节点，实现 AriGraph（arXiv:2407.04363）式的情节-语义混合检索。

### 3.2 Session Compaction

Session 关闭时:
1. LLM 生成 3-5 句会话摘要（高 salience 合成事件）
2. 原始事件保留，不删除
3. 合成事件写入，标记 source='compaction'
4. 后续检索优先返回合成事件

### 3.3 Durative Memory

**DurativeMemoryManager（后台聚类引擎）**:
扫描近期无所属的孤立事件，按时间和语义聚类后使用 LLM 判定连续性，并提取事件总结建立为 DurativeGroup（状态包括 active/closed/archived）。聚类结果通过写入事件关联至 `memory_group_mapping` 避免原地修改。

Consolidate（每小时 cron）:
1. 扫描 30 天内无 durative_group_id 的孤立事件
2. 按语义相似度 + 时间邻近度聚类
3. LLM 判定每个候选簇是否语义连续体
4. 创建 DurativeGroup → Append `memory_group_mapping_created` 事件（event_id → group_id）。禁止原位 UPDATE episodic_events——[EventLog] 受 M11 AuditTrail 哈希链保护
5. 关闭 >7 天无新事件的 active group

读时: LEFT JOIN memory_group_mapping 合成 durative_group_id

`memory_group_mapping` 表 DDL 见 `internal/protocol/schema/003_episodic_memory.sql`。

RetrieveWithDurative:
1. 检测时间意图关键词（"上周"/"三周前"/"当时"）
2. 命中 → 优先在 durative group 摘要层搜索，返回组摘要 + top-3 关键事件
3. 未命中 → 走常规检索

### 3.4 `[ReflectionMemory]` — 元认知反思层

> 对齐 Generative Agents (Park 2023+) 与 MemGPT 反思层共识。Episodic（"发生了什么"）与 Semantic（"普遍事实是什么"）之间的中间层——Agent 自身对**"我做了什么 + 学到什么"**的元认知摘要。

**区别**:

| 层 | 视角 | 内容 | 触发 |
|----|------|------|------|
| Episodic | 事件流水 | "用户问 X, Agent 调 tool_Y, 返回 Z" | Agent 动作即时 |
| **Reflection** | **元认知** | **"在该类任务上 tool_Y 比 tool_W 快 3 倍 + 失败模式 X 来自参数 P 越界"** | **任务终态 + Session 关闭 + 失败 reflection** |
| Semantic | 普遍事实 | "事实图谱: X is_a Y" | Consolidation 合并 |

**与 M9 PersonaRefiner 区别**: PersonaRefiner 更新**用户画像**；ReflectionMemory 更新 **Agent 自身经验**。

**表结构** (`reflection_memory`，DDL 见 `internal/protocol/schema/024_reflection_memory.sql`，实现见 `pkg/cognition/memory/sql_reflection_mem.go`):
- `id` (TEXT PRIMARY KEY), `session_id`, `agent_id`
- `task_type` (TEXT，专用列，`idx_reflect_task_type` 索引覆盖，避免全表扫描)
- `reflection_type` ∈ {success_pattern, failure_mode, efficiency_insight, cross_task_principle}
- `content` (≤500 tokens), `fail_reason`, `strategy`, `decision`
- `salience` (REAL 0-1), `embedding` (BLOB, float16 量化), `embed_model_ver`
- `accessed_count`, `last_accessed_at` (LRU 淘汰依据)
- `evidence_ids_json` (支持该反思的 Episodic 事件 IDs), `meta_json`
- `created_at` (Unix 秒)
- HT0 上限：5000 条，LRU 淘汰批次 100 条

**触发** (后台 worker `ReflectionWorker`，实现见 `pkg/swarm/self_improve/reflection_worker.go`):
1. **任务终态触发**: S_COMPLETE / S_FAILED 进入时，若 task_type 在 `ReflectionConfig.TaskTypeWhitelist` → 投递 reflection job
2. **失败深度反思**: `ReplanCount ≥ ReflectionConfig.MinReplanCount`（默认 2）触发失败模式 LLM 提取
3. 白名单默认值: `["complex_reasoning", "coding", "research", "debug", "analysis"]`，可通过 `NewReflectionWorkerWithConfig` 覆盖

**写入流程**:
1. 收集 Evidence Episodic Events（同 task_id）
2. LLM 提取反思（JSON 严格输出），填充 `reflection_type` + `content`
3. 直接写入 `reflection_memory` 表（`SQLReflectionMem.AppendReflection`）

**读取** (M5 HybridRetriever 第 4 路召回，权重 0.15):
- `HybridRetrieverImpl.reflectionMem` 非 nil 时，第 4 路通过 `ReflectionMemory.QueryReflections(Topic=query)` 走 SQL 索引查询
- `reflectionMem` 为 nil 时降级为 KV 前缀 `reflection:` 扫描（旧部署兼容）
- S_PERCEIVE / S_REPLAN 阶段：`buildPerceiveContext` / `buildPlanContext` 额外直接调用 `QueryReflections(Topic=TaskModel.Goal, K=3)` 注入 system prompt（非 ZoneImmutable——反思为 Agent 自生成，TaintLow，但不走 ContextAssembler 写入门控）
- 与 [HeuristicsMemory] (M9 §2.1) 互补——后者是 task_type→prompt 模板，前者是 task_type→经验摘要

**HT0 限制**: 表大小硬上限 5MB（约 5000 条 reflection），LRU 淘汰最久未访问。得益于 DeepSeek 的极低 API 成本，LLM 提取不再受严苛的财务配额约束，仅受 CPU/内存空闲资源控制（M9 BackgroundTaskScheduler [Priority-2]）。

---

## 4. L2 Semantic Memory

### 4.1 表结构（[Storage-SQLite] 邻接表）

DDL 见 `internal/protocol/schema/004_semantic_memory.sql`。图存储使用 [Storage-SurrealDB-Core]。

### 4.2 UpsertFact

1. searchSimilar(阈值 0.95) → 同一事实，UPDATE 属性 + version++
2. 相似度 > 0.80 → LLM 冲突解决（判断更新 vs 新事实）
3. 相似度 < 0.80 → INSERT 新事实
4. version++ 不可变版本 + source_event_id provenance + 信念修正（矛盾时优先保留更近期/更高证据强度事实）+ Prospective Indexing（写入时预生成未来查询并索引）

**[Entity 生命周期]** `semantic_entities` 新增 `status` 字段（`active`/`superseded`/`expired`/`merged`，默认 `active`）与 `superseded_by` 外键。被取代的旧事实不物理删除，置为 `superseded`；检索路径过滤 `WHERE status='active'`，保留完整信念修订轨迹（来源：PruneMem lifecycle governance）。

- **精确名称冲突**：ON CONFLICT UPDATE 更新属性，同时 `MarkEntitySuperseded(oldDBID, 0)` 将旧行置 `superseded`（直接 SQL，同步执行，非 MutationBus 异步路径）
- **Jaccard 近重复检测**（仅 `user_preference` 类型）：`ListActiveEntities` 取同类活跃实体，对新实体名与各实体名分词求 Jaccard 相似度，`> 0.6` 的实体视为矛盾旧观念，调用 `MarkEntitySuperseded` 打标后再 INSERT 新事实

**[接口约束]** `UpsertFact` 和 `UpsertRelation` 必须作为 `SemanticMemory` 接口的扩展方法在 `internal/protocol/interfaces.go` 中声明，由 `pkg/cognition/memory/semantic_mem.go` 实现；写入必经 `MutationBus.Submit(MutationIntent{Table:"semantic_entities"/"semantic_relations", Op:OpUpsert})`，禁止绕过 M2 单写者约束直接调用 `db.Exec`。生命周期管理方法（`MarkEntitySuperseded`、`ListActiveEntities`、`UpsertUserProfile`、`GetUserProfile`）属于轻量同步读写，走直接 SQL（不经 MutationBus）。Embedding 存 BLOB（float32 → float16 量化，使用 `pkg/substrate/embedding_batcher.go` 的量化工具）。

### 4.3 QueryClassifier + RetrievalRouter

实现见 `pkg/cognition/memory/query_classifier.go`（`ClassifyQuery` 函数）。

**Tier-0（已实现）**: 纯规则中文关键词匹配（<10µs，无 LLM/embedding 调用）：
- 时间关键词（"最近"/"上周"/"历史"）→ `temporal`
- 操作关键词（"如何"/"怎么做"/"步骤"）→ `how_to`
- 事实关键词（"是什么"/"定义"/"谁是"）→ `factual`
- 推理关键词（"为什么"/"分析"/"比较"）→ `reasoning`
- 规则未命中 → `unknown`（等效全搜）

**Tier-1+（待实现）**: 查询 embedding 与 4 个类型原型向量余弦相似度比较（置信度 <0.3 时回退 `unknown`）。Tier-0 成本约束禁止此处调用 embedding API。

**RetrievalRouter（已实现，在 HybridRetriever.Search 内）**:
- `temporal` → 激活第 5 路 `DurativeMemory`（`DurativeMemoryManager.RetrieveGroups`）
- 其余类型 → 标准 BM25 + Simhash + Graph + ReflectionMem 四路融合
- RRF 权重: BM25=1.0 / Simhash=0.8 / Graph=0.6 / Reflection=0.15 / Durative=0.3

---

## 5. L3 Procedural Memory

程序记忆（技能库）采用三层存储架构: SurrealKV KV 作为热路径的签名级精确查找（skill_id → Wasm 二进制，延迟 <10µs），SurrealDB-Core 作为语义搜索路径（用于 System 2 的 embedding-based 相似技能检索），文件系统 SKILL.md 作为 Ground Truth（技能源码和契约的权威定义，受 Git 版本控制）。

双轨检索: System 1 路径通过 IntentSignature 在 SurrealKV 中做 O(1) 精确匹配（亚毫秒级），命中后由 M6 WasmSkillCache 缓存编译产物直接执行。System 2 路径在 SurrealDB-Core 中做 KNN 语义搜索，返回候选技能后按成功率排序，渐进披露注入 LLM prompt。

L3 Procedural 技能索引相关 DDL 实质托管于 M2 SurrealKV KV 引擎（`pkg/substrate/storage/SurrealKV.go`），SKILL.md 元数据从文件系统懒加载。M5 skillKV 与 M6 WasmSkillCache 的关系见 M6 §5.1。

---

## 6. Write Path: Hot/Cold 分离

Agent 动作完成后，写入路径拆分为两条线:

**热路径（同步，<10ms）**: 纯文本事件日志写入 EventLog（SQLite WAL，约 100µs），同步更新 Working Memory 缓存，触发 TokenBurnRate 计数。不调用 LLM、不操作图、不等 embedding API——保证 Agent 回复不受后台处理延迟影响。

**冷路径（M2 Outbox Worker 异步）**: Outbox Worker 消费事件日志后异步执行——调用 M1 Embedding API 生成向量、写入 SurrealDB-Core 索引、提取实体和关系 Upsert Semantic Memory、检查 Consolidation 阈值并触发压缩、更新 Skill 成功率统计。

热路径仅做纯文本日志写入，不调用 LLM、不操作图、不等 embedding。冷路径在后台静默运行。

---

## 7. Read Path: HybridRetriever

实现见 `pkg/cognition/memory/retriever.go`（`HybridRetrieverImpl`）。

```
HybridRetrieverImpl (pkg/cognition/memory/retriever.go):
  store         protocol.Store          — KV 层（episodic:/chunk: 前缀扫描）
  graph         GraphTraverser          — Tier1+，nil 跳过
  durative      *DurativeMemoryManager  — 第 5 路，temporal 查询激活，nil 跳过
  reflectionMem protocol.ReflectionMemory — 第 4 路 SQL 路径，nil 降级 KV 扫描

构造器:
  NewHybridRetriever(store)                          — 基础版（仅 BM25+Simhash）
  NewHybridRetrieverWithGraph(store, graph)           — Tier1+ 图路径
  NewHybridRetrieverWithDurative(store, graph, dur)  — +DurativeMemory 第 5 路
  NewHybridRetrieverFull(store, graph, dur, reflMem) — 全路径（NewMemImplWithDB 默认使用）
```

### 7.1 BM25Index

- 存储: [Storage-SurrealDB-Core] BM25 (k1=1.2, b=0.75)
- Tokenizer: SurrealDB-Core + jieba-rs / lindera 多语言分词器；M3 启动期检测系统 locale，自动选择默认分词器
- Search: 分词 → FTS5 MATCH → BM25 分数降序 topK → 批量加载完整事件
- 冷路径异步 Index(event): Unicode 标准化 + 词干提取 + 停用词过滤 → 写入倒排索引

### 7.2 VectorIndex + Simhash

- 存储: [Storage-SurrealDB-Core] vec0(float32[4096])，k-means 分区 (k=sqrt(N), 每 10K 向量重分区)
- Search: embedder.Embed(query) → KNN 余弦距离 → topK
- **Simhash 备选**: 64-bit Simhash 指纹（纯 Go, <10µs/text），embedding API 不可用时降级为 Simhash 扫描（汉明距离 ≤8）→ BM25 + Simhash + GraphTraverse 三路融合替代 DenseVec 路径。非替代，为廉价近似

### 7.3 GraphTraverser + Spreading Activation

Traverse（BFS）:
1. 种子实体: semantic_entities 中与 query embedding 最相似 top-5
2. 有界 BFS (depth=2)，entity_type 加权: Person ×2.0, Project ×1.5, Tool ×1.0
3. 去重，按路径权重之和降序
4. 限制: maxNeighborsPerHop=20, maxTotalNodes=200

Spreading Activation（关联发现模式）:
1. top-3 种子实体，activation_energy=1.0
2. 扩散: energy × edge.weight 传播至邻居，自身 ×0.5 衰减。≤0.05 停止
3. 最多 5 轮
4. 按最终 energy 降序 topK
5. 模式选择: query 含"为什么"/"原因"/"关联"/"影响" → Spreading Activation; 否则 BFS

### 7.4 RRF 融合 + BM25 精排

当前实现为 5 路 RRF 融合（`pkg/cognition/memory/retriever.go`，`HybridRetrieverImpl.Search`）：

```
Stage 0 — scope 路由:
  scope.Type == "memory"  → prefix = episodic:
  scope.Type == "chunk"   → prefix = chunk:
  隐私门控由上层 M11 Policy Gate 在 ctx 中注入，retriever 不内联 ACL

Stage 1 — 5 路宽召回:
  路径 1: BM25（weight=1.0）
    KV 前缀扫描 → 词频 TF×IDF 近似 BM25 打分（Tier 0 纯 Go，无 FTS5 依赖）
  路径 2: Simhash（weight=0.8）
    64-bit Simhash 指纹，汉明距离 ≤16 → simScore=1-(dist/64)
  路径 3: Graph（weight=0.6，Tier1+，graph==nil 时跳过）
    BM25 Top1 作起点 → GraphTraverse(depth=2) → 跳数衰减赋分
  路径 4: ReflectionMemory（weight=0.15，scope=memory）
    reflectionMem != nil → SQL QueryReflections（idx_reflect_task_type 索引加速）
    reflectionMem == nil → 降级 KV 前缀扫描 reflection:（旧部署兼容）
  路径 5: DurativeMemory（weight=0.3，scope=memory 且 ClassifyQuery==Temporal）
    durative.RetrieveGroups(query, 5) → Label+Summary 参与 BM25 打分

Stage 2 — RRF 融合:
  score(d) = Σ weight_i / (k + rank_i + 1)，k 见 `spec/state.yaml §m5_memory.rrf_k`（默认 60），按各路 rank 累加后降序

Stage 3 — 截断: FinalTopK=20（默认，config.FinalTopK 可覆盖）
```

**M5/M10 共享关系**：检索范围不同（M5: episodic+semantic 前缀，M10: doc_nodes/chunk: 前缀），`HybridRetrieverImpl` 通过 `scope.Type` 参数路由，无需两套实例。M10 参数差异见 M10 §2.2。

**跨版本嵌入兼容**：`MemoryEntry.EmbedModelVersion` 字段触发 `OnlineReindexer`（inv_M5_03）；当前 Tier 0 BM25+Simhash 路径不依赖 embedding API，嵌入模型变更不影响 Tier 0 召回。

**OnlineReindexer（批量重建 embedding 索引）**：

实现见 `pkg/cognition/memory/online_reindexer.go`（`OnlineReindexer` struct）。

- **触发条件**：`episodic_events.embed_model_version = ''`（OutboxWorker 尚未投影）或 `!= currentVersion`（模型切换）；走 `idx_ep_embed_ver` 偏索引，O(1) 量级扫描
- **接口**：`Embedder`（consumer-side，`Embed(ctx, text) ([]float32, error)` + `ModelVersion() string`）——M1 EmbeddingBatcher 实现此接口注入，防包循环
- **批处理**：`Run(ctx)` 每次取 50 条（`defaultReindexBatchSize`），每条 embed 后 `runtime.Gosched()` 让出调度，单条失败不中断整批（best-effort）
- **量化**：float32 → float16 BLOB（IEEE 754 half-precision，小端序），与 DDL 003/004 规范一致；精度损失在 RRF 归一化路径中可接受
- **返回**：`(processed int, remaining bool, err error)`；调用方循环调用直至 `remaining=false`；version 切换场景由调用方决策重触发，避免无限循环

### 7.5 Evidence Subgraph Extraction

**EvidenceSubgraphExtractor**:
负责知识图谱的子图提取。基于种子实体执行有界 BFS 和 Personalized PageRank（PPR）随机游走评估相关性，最终输出结构化子图上下文，辅助跨多跳关系的证据链检索。

### 7.6 Edge Weight Reinforcement & Decay

**EdgeWeightManager**:
控制图谱边权重的生命周期。提供读取时基于时间衰减的 O(1) 计算模型（防写放大）；同时对图遍历与成功路径访问提供按权增加强化校准，支持定期的无用图边清理。

---

## 8. Effective Connectivity（冷后台预计算）

`semantic_connectivity_cache` 表 DDL 见 `internal/protocol/schema/004_semantic_memory.sql`（派生数据缓存，非事实源）。

Effective Connectivity 被预计算为可 O(1) 查询的缓存表。ConnectivityPrecomputer 由 M9 BackgroundTaskScheduler 在每日凌晨 4:30 触发（与 Consolidation 3:00 错开 ≥90 分钟）。Tier 0 最多计算 200 个种子实体（约 20MB 内存），Tier 1+ 扩展到 1000 个。分批 50 实体/批，批间释放 CPU（Gosched），CPU 占用 >30% 或空闲内存 <2GB 时挂起。采用 INSERT OR REPLACE 全量覆盖旧缓存。

ActivationMaximization 查询时 O(1) 完成——任务 embedding 搜索最相似的 20 个实体 → 按预计算的 effective_weight 排序 → 取 topK + BFS+PPR 构建最小激活子图。

---

## 9. Consolidation

**触发条件**:
- 主题转换检测到 shift → 立即触发
- eventCount ≥ 50 → 触发，计数归零
- sessionClosed → 强制触发

**5-Stage Pipeline** (`pkg/cognition/consolidation.go`):

- Stage 1 — **实体/关系提取**: 聚合 Session 内 Episodic 事件文本，优先调用 Provider LLM 提取命名的限定实体（`user_preference`, `constraint`, `temporary_conclusion`, `entity`）与特定关系（`depends_on`, `configures`, `conflicts_with`, `relates_to`），结构化输出 JSON。LLM 不可用时降级为正则模式匹配。
- Stage 2 — **Upsert Semantic + Entity 生命周期**: 批量调用 `SemanticMemory.UpsertFact / UpsertRelation`。精确名称冲突 → `MarkEntitySuperseded(oldDBID)` 打标 `superseded` 后 INSERT 新版本；`user_preference` 类型还额外执行 Jaccard 近重复检测（阈值 0.6），Jaccard 命中的旧实体同样打标 `superseded`。单条失败不中止批次。
- Stage 3 — **会话摘要**: LLM 生成 3-5 句摘要（source='compaction'），写入 `SemanticMemory.StoreDocument`；LLM 不可用时降级为事件类型频次拼接摘要。
- Stage 3.5 — **UserProfile 合成（L3 Persona）**: `len(events) >= 10` 时触发。优先 LLM 路径（512 token 预算）从 Episodic 事件批量提取 `stable_facts`/`recent_activity`/`behavioral_patterns`；LLM 不可用时降级为规则路径（工具调用频次 + 事件类型统计）。结果写入 `user_profile` 表（`UpsertUserProfile`，ON CONFLICT UPDATE，`profile_key='default'`）。非阻塞，失败静默跳过。来源：supermemory 用户画像持久化方案。
- Stage 4 — **Logic Collapse → Skill Library**: 统计 Session 内同名工具成功调用次数，≥ 3 次时将该工具注册为 `SkillMeta`（`SkillRegistry.Register`）；SkillRegistry 为 nil 时跳过。

关键约束: version++ 不可变版本 + source_event_id provenance + 信念修正 + 单条失败不中止整体管线

**依赖注入**: `NewConsolidationPipeline(episodic, semantic, skills, provider)` — episodic/semantic 必须非 nil；skills 和 provider 可选（nil 时对应阶段降级或跳过）。

---

## 10. Forgetting: 双层策略

### 10.1 热删除：效用衰减

记忆的效用随时间按指数衰减。衰减公式: `salience × exp(-decayRate × ageHours/24)`，decayRate = 0.01/日。衰减权重低于 salienceThreshold（默认 0.15）时写入 tombstone 标记（key=`forgettable:{id}`），不物理删除原事件。Q-Learning 熵门控（`QLearner`）动态调整阈值——高熵任务降低阈值保留更多历史，低熵任务提高阈值加速遗忘（α=0.1, γ=0.9）。

Forgettable 事件保持原地，由 ColdArchiver.PhysicalCompact 负责最终清理。

### 10.2 冷归档

标记为 Forgettable 且年龄超过 30 天的事件移入归档前缀（`archive:episodic:{id}`），原始 key 和 tombstone 同时删除。支持 SQL 的引擎（`StoreCapabilities.SupportsSQL=true`）触发 VACUUM 回收磁盘。归档数据通过前缀扫描按需回查，无删除限制。

`PeriodicCleanup` 的扫描优化：如果底层 KV 层支持原生 SQL（通过 `dbAccessor` 断言），后台会直接通过 SQL `SELECT id, salience, occurred_at FROM events WHERE topic IN ('memory.openclaw', 'memory')` 提取评估目标，彻底避免缓慢的全表遍历与耗时的 ULID 解析。
`PhysicalCompact` 扫描 `forgettable:` 前缀批量清理——两步分离保证单次扫描不阻塞写路径。

---

## 11. ContextAssembler

BuildContext 5 Zone 布局和 SessionCompressor 实现见 `pkg/cognition/context_assembler.go`。

### 11.0 系统提示词三层组装

系统提示词在 `ImmutableCore.PrependToMessages()` 中按三层顺序组装，对应 KV Cache 由稳定到易变的排列原则：

| 层 | 内容 | 变更频率 | 文件 |
|----|------|---------|------|
| stable | Agent 身份（SOUL.md 或 `DefaultPolarisIdentity`）+ 模型感知工具调用引导 + 平台感知提示 | 跨会话不变 | `pkg/cognition/memory/identity.go` `pkg/cognition/memory/platform_hints.go` |
| context | 项目上下文（由 M13 Interface 层注入 ambient skills / 全局目标等） | 会话内不变 | `pkg/gateway/server/sse.go` |
| volatile | 当前日期（精确到天，不到分钟，避免逐分钟破坏 prefix cache） | 每天变一次 | `ImmutableCore.VolatileBlock` |

**用户自定义身份**：`~/.polarisagi/polaris/config/SOUL.md` 存在时覆盖 `DefaultPolarisIdentity`，服务启动时一次性读取并缓存到 `Server.soulMDContent`。

**模型感知引导**：`memory.NeedsToolUseEnforcement(modelID)` 判断模型族（deepseek/qwen/gpt/gemini 等），对应注入 `memory.ModelSpecificGuidance(modelID)` 的专属工具调用约束文本，防止模型仅描述意图而不实际调用工具。

**平台感知提示**：`memory.PlatformHintFor(platform)` 按接入平台（cli/webui/api/cron）注入不同的输出格式和行为说明（定义见 `pkg/cognition/memory/platform_hints.go`）。`POLARIS_PLATFORM` 环境变量控制，默认 `webui`。

**M9 激活路径**：`PromptVersionStore.OnActivate` 回调在 `task_type='general'` 的版本激活后触发，热更新 `Server.activatedSystemPrompt`，下一轮 `injectSystemPrompt()` 生效（goroutine-safe，受 `activatedSystemPromptMu` 保护）。`SystemPromptTemplate` 非空时全量走模板渲染，跳过三层组装，允许 M9 完全接管系统提示词内容。

### 11.1 三所有权层

提示词所有权明确分为三层，变更边界清晰：

| 层 | 存储位置 | Owner | 变更方式 | DB 重置（re-init）行为 | Factory Reset 行为 |
|----|---------|-------|---------|---------------------|------------------|
| **Layer 0** 内置默认 | `configs/prompts/*.md`（go:embed，随二进制） | Polaris 项目 | PR + 重新编译 | 不受影响（embedded） | 不受影响 |
| **Layer 1** 用户自定义 | `~/.polarisagi/polaris/config/prompts/*.md` | 用户 | 直接编辑文件 或 `PUT /v1/config/prompts/{name}` | **存活**（文件不在 DB 中） | `DELETE /v1/config/prompts/{name}` 显式删除 |
| **Layer 2** M9 优化 | DB `prompt_versions` 表 | 自进化引擎 | M9 自动生成 + Staging 审批 | **重置**（随 DB 删除，正确） | 随 DB 删除 |

**用户可通过 API 编辑的提示词**（Layer 1，白名单控制）：
- `identity`：Agent 身份文本（覆盖 Layer 0 identity.md，整段替换）
- `custom_instructions`：追加的行为指令（拼接到身份之后，不覆盖）

不暴露给用户的提示词：tool_enforcement（产品行为逻辑）、platform hints（格式化指令）。

**API**: `GET/PUT/DELETE /v1/config/prompts/{name}`（实现见 `pkg/gateway/server/prompts.go`）。

**re-init vs factory reset**：删除 DB（re-init）只清 M9 数据，Layer 1 用户文件存活；`DELETE /v1/config/prompts/{name}` 才清用户自定义，恢复到内置默认。

**Prompt 组装顺序与 KV Cache 优化规范**:
为了最大化利用支持 Prompt Caching (如 Anthropic/DeepSeek) 模型的 KV Cache 命中率，Prompt 的内部块组装必须遵循严格的静态从长到短顺序，保证最久不变的块位于最前部：
`ImmutableCore → Procedural (Skills) → Semantic (Knowledge) → Episodic (Recent Events) → Working (Scratchpad) / TaintedData`
1. **ImmutableCore**: 系统级常量与动态组装的系统提示词（含 Agent 身份认知、运行模型、工具/插件摘要及全局目标），置于首位且永不被裁剪。
2. **Procedural**: 当前 Agent 挂载的工具和技能声明，在特定任务会话内保持稳定。
3. **Semantic & Episodic**: 随会话推进按步增量追加，更新频次中等。
4. **Working / TaintedData**: 高频读写的临时草稿和不可信外部输入，置于最后，不参与 Cache。
*注：M1 Provider Adapter 会自动探测该顺序，向稳定块的末尾段落注入 `cache_control: {"type":"ephemeral"}` 以激活缓存（详见 M1 §3）。*

Layout Zone → ContextZone 映射表、安全约束和不变量见上文 §2.1。

### SessionCompressor

**SessionCompressor** 实现见 `pkg/cognition/compressor.go:SessionCompressor`。

> 与 M4 ContextWindowManager 协同：M4 §7 持有热路径阈值（>70% salience 排序候选 / >90% 语义结构感知逐出），M5 SessionCompressor 在 M4 调用时执行实际的冷压缩算法（本节定义）。

压缩 Stage（由 M4 ContextWindowManager 调用，不独立设阈值）:
- **Stage 1**: tool output pre-pruning——超过 10KB 的 `tool_result` 替换为存根 `[offloaded: N bytes → read_tool_ref("xxx")]`，原始内容经 `ToolRefOffloader` 落盘；立即释放 token，可按 node_id 按需回取
- **Stage 2**: LLM 锚点摘要——以 currentSummary 为锚点追加新事件产生增量摘要（由上层 M4 调用后写入 `SetAnchor`）
- **Stage 3**: **TaskMermaidCanvas 注入**——将 `TaskMermaidCanvas.Render()` 输出（`graph LR` 有向图）前置注入 anchor，形成 `## Task State (node_id → read_tool_ref)\n{mermaid}\n## Summary\n{anchor}` 结构；画布为空时跳过注入。来源：TencentDB Agent Memory 符号化短期记忆（61% token 节省原理）

**TaskMermaidCanvas** (`pkg/cognition/mmd_canvas.go`): 线程安全符号画布。`TrackToolCall(toolUseID, toolName)` 记录 pending 节点；`TrackToolResult(toolUseID, success, summary)` 完成节点并自动连边；`Render()` 输出 Mermaid `graph LR`，成功节点绿色（`fill:#4a4`），失败节点红色（`fill:#d64`）。节点上限 30，summary 截断至 40 字符。估算 token 约 8 token/节点，20 节点画布 ~160 token。

锚定策略: 架构决策、失败原因、修复方案、用户风格偏好永久保留；当前进度和待办事项允许更新；具体工具输出允许丢弃。

---

## 12. Embedding Drift 对策

### 12.1 维度切换无缝过渡

当 M1 Embedder 维度发生变化（或切换为 local_only）时：
- 检索路由: M5 检测当前 Provider 维度，动态将查询路由至 SurrealDB-Core 对应维度的独立隔离表（如 index_remote_4096 或 index_local_384）。
- 避免降级: 维持 DenseVec 默认权重，禁止因为维度不兼容而退化为纯 BM25。
- M6 同步: 同理，M6 通过访问独立维度表维系 L1 vecIndex 权重，避免回退到 FTS5/BM25 替代方案。

### 12.2 Blue-Green Index Swap

Tier 0 前提条件: 空闲内存 > 2 × 当前向量索引占用 (通过 M3 sysinfo.FreeMemory() 校验)。不满足时退化为原地增量更新。

```
1. 后台构建: SurrealDB-Core 创建新版本索引 (index_v{N+1})，查询仍用 index_v{N}
2. 质量验证: 锚定样本(100 条) Recall@5 ≤ 旧索引 90% → ABORT + WARN
3. 原子切换: SurrealDB-Core 索引版本指针原子更新 → 查询路由至新索引 (<1ms)
4. 旧索引: 异步回收 index_v{N}
5. FTS5/BM25 始终在线
```

### 12.3 DriftDetector

```
DriftDetector:
  anchors(100 AnchorSample), checkInterval(7d), driftThreshold(0.05)
Detect:
  1. sampleCount<5 任务类型跳过 (标记 unknownCount++)
  2. 重新检索 → 计算 Top-5 变化率 + 余弦距离变化
  3. changeRate>0.4 且 cosineDelta>driftThreshold → 记录漂移
  4. unknownRatio>0.30 → 系统级告警
EmbeddingVersionTracker:
  每索引维护 P50/P95/P99/Min/Max 滚动统计(EWMA alpha=0.01)
  跨版本检索: min-max 归一化 → RRF 融合
```

---

## 14. 降级与失败模式

- 故障场景: HybridRetriever 单路检索失败 | 降级路径: 其余路权重接管（DenseVec 失败 → BM25×0.7 + Graph×0.3） | 恢复策略: 故障路恢复后自动切回默认权重
- 故障场景: Embedding API 不可用 | 降级路径: Simhash 64-bit 指纹备选（汉明距离 ≤8）+ BM25 + 图遍历三路融合 | 恢复策略: API 恢复后 DenseVec 权重切回
- 故障场景: SurrealDB-Core 维度切换 | 降级路径: 动态路由查询至对应维度表（如 index_local_384），无需强制降级 BM25 | 恢复策略: 后台静默回填增量
- 故障场景: Consolidation LLM 调用超时 | 降级路径: 跳过本轮 Consolidation，事件保留在 episodic_events 等待下一轮 | 恢复策略: 下个 cron 周期自动重试
- 故障场景: NotesStore CAS 乐观锁冲突超限（>3 次） | 降级路径: 写入 notes_conflict_log shadow 表供人工裁决 | 恢复策略: —
- 故障场景: Episodic 冷路径 Outbox 积压 | 降级路径: 暂停非关键 Consolidation + WARN | 恢复策略: 积压降至 <200 恢复正常
- 故障场景: Mem-L3 SurrealKV 引擎故障 | 降级路径: 降级 SQLite 备份索引 (skill_id→metadata，不含 blob) | 恢复策略: SurrealKV 恢复后切回
- 故障场景: DurativeMemory 聚类 LLM 超时 | 降级路径: 按纯向量余弦相似度聚类（跳过 LLM 语义判定） | 恢复策略: LLM 恢复后追加语义连续性标注
- 故障场景: ContextAssembler 组装超时 (>500ms) | 降级路径: 跳过 ZoneMutableSkill 组装，仅发 ZoneImmutable + 摘要 | 恢复策略: 下次 context refresh 完整组装
- 故障场景: EdgeWeightManager 图边修剪 LLM 超时 | 降级路径: 仅物理 DELETE < pruneThreshold 的边，跳过 FeedbackCalibrate | 恢复策略: 下个 cron 周期重新校准
- 故障场景: Embedding DriftDetector 检测到漂移 | 降级路径: 该 task_type 降级纯 BM25，其余不受影响 | 恢复策略: Blue-Green 重嵌完成后切回

与 OSMemoryGuard 协同: L1 预警 → 暂停 Consolidation 冷路径 / L2 紧急 → 暂停 Episodic 冷路径 Outbox 处理、限制 WorkingMemory 容量 / L3 临界 → 全部冷路径暂停，仅热路径写入 + L0 WorkingMemory 读取可用。

---

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m5_memory`。

## 15. 跨模块依赖与契约

- 关联模块: M1 Inference | 关键契约: Embedding API（向量生成）、LLM 调用（摘要/Consolidation） | 位置: M1 §6.1, §5
- 关联模块: M2 Storage | 关键契约: Store 接口、EventLog 真相源（events 表 → episodic_events 派生投影） | 位置: M2 §1.1, §2.1
- 关联模块: M4 Agent Kernel | 关键契约: ContextAssembler、HybridRetriever 上下文检索 | 位置: M4 §2, §10
- 关联模块: M6 Skill Library | 关键契约: L3 Procedural Memory 技能索引 + SurrealKV 缓存 | 位置: M6 §7
- 关联模块: M9 Self-Improve | 关键契约: M9→ZoneMutableSkill Taint Gate（双层）、Preference Learner、PersonaRefiner | 位置: M9 §1.1, M5 §2.1
- 关联模块: M10 Knowledge RAG | 关键契约: HybridRetriever 共享引擎（pkg/substrate/hybrid_retrieve.go）、检索配置差异 | 位置: M10 §2.2
- 关联模块: M11 Policy Safety | 关键契约: SafetyRules 注入 ImmutableCore、TaintGate 写入 Zone 校验 | 位置: M11 §2, M5 §2.1
- 关联模块: 全局字典 | 关键契约: HE-Rule-4 数据驱动迭代、HybridRetriever/RRF 定义 | 位置: 00-Global-Dictionary §2, §9-bis
- 关联模块: DDL | 关键契约: 001_events（真相源）、003_episodic_memory（派生投影）、004_semantic_memory（语义层） | 位置: internal/protocol/schema/001-004_*.sql

---

## 16. 实现状态与 2026 研究对照

### 实现记录

| 组件 | 文件 | 状态 |
|------|------|------|
| L2 SemanticMem | `pkg/cognition/memory/semantic_mem.go` | **✅ 已完成** — 已重构 `UpsertFact`/`UpsertRelation` 接口对接 `MutationBus` 写路径，并实现基于 `*sql.DB` 的 `GetEntity` 直读查询，淘汰了旧版纯 JSON KV 占位 |
| Entity 生命周期 | `pkg/cognition/memory/semantic_mem.go` `internal/protocol/schema/004_semantic_memory.sql` | **✅ 已完成** — `semantic_entities` 新增 `status`/`superseded_by` 字段；实现 `ListActiveEntities`、`MarkEntitySuperseded`；Consolidation Stage 2 引入 Jaccard 近重复检测（阈值 0.6，`user_preference` 类型）；检索路径过滤 `status='active'` |
| UserProfile 合成 | `pkg/cognition/consolidation.go` `internal/protocol/schema/004_semantic_memory.sql` | **✅ 已完成** — `user_profile` 新表（`stable_facts`/`recent_activity`/`behavioral_patterns` JSON 字段）；Consolidation Stage 3.5，`events≥10` 触发，LLM 路径（512 token）+ 规则降级；`UpsertUserProfile`/`GetUserProfile` 接口实现 |
| TaskMermaidCanvas | `pkg/cognition/mmd_canvas.go` `pkg/cognition/compressor.go` | **✅ 已完成** — 线程安全 Mermaid `graph LR` 画布；`SessionCompressor` 新增 Stage 3 注入逻辑；12 个单元测试覆盖节点样式、截断、Jaccard、Compressor 集成 |
| EpisodicGraphBridge | `pkg/cognition/memory/episodic_graph_bridge.go` | **✅ 已完成** — `ACTION_DONE` 事件已实现解析 `event.Payload` 提取真实 `tool_name` 构建动态图边，移除硬编码 |
| ReflectionMem | `pkg/cognition/memory/reflection_mem.go` | **✅ 已完成** — 实现了基于 HT0 限额的 LRU 缓存驱逐与存取机制 |
| ContextAssembler | `pkg/cognition/context_assembler.go` | **✅ 已完成** — 实现了针对 TaintData 且 TaintMedium 以上的 M11 Spotlighting 封锁包装 |
| DurativeMemoryManager | `pkg/cognition/memory/durative_mem.go` | **✅ 已完成** — 实现了持续性记忆组 `DurativeGroup` 的聚类合并引擎 |
| EdgeWeightManager | `pkg/cognition/memory/edge_weight.go` | **✅ 已完成** — 实现了动态读时图边衰减（`DecayUnused`）以及 BFS 子图抽取引擎 |
| ReflectionWorker | `pkg/swarm/self_improve/reflection_worker.go` | **✅ 已完成** — 实现后台提取，在 session 完成后执行经验萃取入库 |

### 引入计划（优先级排序）

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **D-MEM 多巴胺门控巩固** | arXiv:2603.14597, 2026 | 以 SurpriseIndex 信号作门控，仅 surprise > 阈值的情节事件才晋升语义层，消除冗余写入与 O(N²) 延迟 | `consolidation.go` §9 + `surprise.go` | P1 |
| **Path-Constrained Retrieval** | arXiv:2511.18313, 2025 | BFS 遍历限定关系类型白名单（uses/depends_on/extends），防止多跳推理语义漂移 | `pkg/substrate/hybrid_retrieve.go` GraphTraverse | P2 |
| **E-mem 多 Agent 情节重建** | arXiv:2601.21714, 2026 | 异构辅助 Agent 维护未压缩情节上下文，token -70%，F1 +54%；当前单节点情节记忆在 swarm 场景是盲点 | `pkg/swarm/orchestration.go`（中期） | P3 |
