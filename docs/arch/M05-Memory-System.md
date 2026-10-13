
# 模块 5: Memory System

> 四层记忆（Working / Episodic / Semantic / Procedural），多存储引擎绑定，[Tier-0-Limit]
> Go（记忆管理器 + 检索路由 + Consolidation），Rust（Embedding 计算 via M1）
> [HE-Rule-4] [HE-Rule-5] [HE-Rule-6]
<!-- §跳读: 0-bis:8 职责 / 0-ter:20 不变量速查 / 1:35 四层映射 / 2:46 L0 Working / 3:115 L1 Episodic / 4:174 L2 Semantic / 5:204 L3 Procedural / 5-bis:214 Memory-Write-Tool(Agent主动写) / 6:256 写路径 / 7:264 HybridRetriever / 8:324 EffConn / 9:330 Consolidation / 10:354 Forgetting / 11:368 PromptBuilder / 12:416 571 Drift / 14:439 609 (SOFT)降级 / 15:463 633 依赖 / 16:479 649 实现状态+2026研究对照 / 13:495 675 MemoryAgent(Swarm集成) -->
## 0-bis. 职责边界

- M5 **是**: 四层记忆读写管理器 | **不是**: 物理存储引擎（M2）
- M5 **是**: HybridRetriever 检索路由（BM25 + Dense + Graph → RRF） | **不是**: Embedding 向量计算（M1）
- M5 **是**: Consolidation 语义压缩（episodic→semantic） | **不是**: Skill 执行（M6）
- M5 **是**: ImmutableCore 用户长期偏好管理（永不裁剪）+ UserProfile 自动合成（§9 Stage 3.5） | **不是**: 安全策略决策（M11）
- M5 **是**: Context 管理与组装（`kernel.PromptBuilder` 承担 Slot 分离 + Taint 门控）+ 任务画布符号化压缩（§11.3） | **不是**: Agent 任务调度（M4）
- M5 **是**: Forgetting 策略（效用衰减 + 冷归档） | **不是**: 物理数据删除（委托 M2）
- M5 **是**: Entity 生命周期治理（active/superseded/expired）+ Jaccard 矛盾检测（§4.2） | **不是**: 策略执行（M11）

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M5_01 | ImmutableCore 永不参与压缩——ContextWindow.Compress 跳过此区域 | CI（Continuous Integration，持续集成） `immutable_core_integrity` 测试 |
| inv_M5_02 | ImmutableCore 写入必经 staging 审批——`provenance_id` CHECK 约束 | DDL CHECK + M11 审计 |
| inv_M5_03 | embed_model_version 是一等字段——每 chunk/event 携带，跨版本检索走 OnlineReindexer | DDL NOT NULL 约束 |
| inv_M5_04 | 默认归档不物理删除——`archived=1` 级联 chunks/entities，GDPR Art.17 例外 | M2 Forgetting 审计 |
| inv_M5_05 | RRF 融合不裸加权——`weight/(k+rank+1)`，防止不同检索器分数尺度不可比 | HybridRetriever 代码审计 |
| inv_M5_06 | Taint 传播——chunks/events/RetrievedItem 均携带 5 级 TaintLevel，只升不降 | CI `taint_propagation` 测试 |

> 进入本模块前必读 RAG 链路 + 向量检索工程化陷阱：`docs/specs/09-LLM-Agent-Production.md`（A-07/A-08/A-14 + P-4/P-5）。

---

## 1. 四层映射

| 记忆层 | 物理存储 | 读写比 | 延迟要求 |
|--------|----------|--------|---------|
| L0 Working | 进程内 ContextWindow(Slice) + ScratchPad(sync.Map) [Tier-0] + Immutable Core + NotesStore(SQLite 持久化) | 1:1000 | <1µs |
| L1 Episodic | [Storage-SQLite] session_events + [Storage-SurrealDB-Core] embedding 列 | 100:1 | 写<100µs, 读<5ms |
| L2 Semantic | [Storage-SurrealDB-Core] | 1:50 | <10ms |
| L3 Procedural | [Storage-SurrealDB-Core] skill_id→blob + 语义检索 + 文件系统 SKILL.md | 1:500 | <10µs / <5ms |

---

## 2. L0 Working Memory

### 2.1 核心结构

WorkingMemory/ImmutableCore/ContextWindow/ScratchPad 接口见 `internal/protocol/interfaces_memory.go`，实现见 `internal/memory/`；UserProfile（PersonaRefiner）实现见 `internal/agent/context/`。

**写入权分离**: M11 Policy → ImmutableCore.SafetyConstraints；M9 PersonalizationWorker → ImmutableCore.UserPreferences + InteractionSummary；用户显式 `/set` → UserPreferences；M5 仅读取，永不写入。

**ContextZone**（`internal/protocol/prompt_builder.go`）:
- `ZoneImmutable=0`：系统全局目标、Agent 身份认知（含底层模型及工具列表）、用户长期偏好与安全约束；置顶，永不裁剪
- `ZoneCoreMemory=1`：核心工作记忆区，Agent 可显式编辑的持久化区块集合（persona, task_state 等）
- `ZoneMutableSkill=2`：SKILL.md 描述模板，M9 PromptOptimizer 的合法优化靶点
- `ZoneTaintedData=3`：外部数据，[TaintLevel] Tracked，永不进入指令区

**MemFS —— 显式可编程记忆块（ADR-0033，原 0082）**：`core_memory_edit` 工具 operation 集合 `["list","get","set","append","replace","delete","describe"]`。`replace` 要求 `old_str` 唯一匹配（除非 `replace_all=true`）；`describe` 记录块用途。`core_memory_blocks` 表（`034_core_memory.sql`）含 `description` / `read_only`（保护块，拒绝一切写）/ `max_bytes`（单块上限，新建行时固化，不追溯）。容量三层：单块 `max_bytes`、总量 `core.memory_total_max_kb`、块数 `core.memory_max_blocks`（=16，`state.yaml §m5_memory`）。`list` 返回 JSON 不含 `content`，防止一次 list 灌满上下文。全部 operation 经既有 `ExecuteTool` 路径，无旁路。

**M9 → ZoneMutableSkill Taint Gate（双层）**:
1. **自动层**: PromptOptimizer 输出 → M11 `SanitizeBySchema` + `SanitizeByDeterministicTransform` → SIC 检测 → 阳性丢弃 + 审计 `prompt_opt_taint_rejected`
2. **HITL 层**: 前 5 次写入经 LLM-as-Judge 语义审核，unsafe → [ESCALATE]；累计 5 次通过后概率抽查（20%/次，安全随机源），优先抽"语义距离 >2σ"输出（历史 PromptOptimizer embedding 分布）；命中 + unsafe → 撤销该 task_type 全部豁免 + 语义漂移分析（余弦距离 >2σ → CRITICAL）。禁止永久豁免
3. **独立 LLM-as-Judge 二次审查**: 合并前必经不同 Provider 模型的独立 Judge；不通过 → 丢弃 + HITL（防 SIC 对间接指令注入的 false negative）

**kernel.PromptBuilder 写入门控**:
0. **InteractionSummary 特例**: M9 PersonaRefiner 生成的 InteractionSummary（`source='persona_refinement'` + Ed25519 签名）写 ZoneImmutable 前执行 `SanitizeByDeterministicTransform`（<200 tokens 摘要 + SHA-256 校验和），TaintLevel 强制 TaintLow。固定白名单——仅该 source + 有效 M9 签名可写 ZoneImmutable
1. zone==ZoneImmutable 且 TaintLevel > TaintLow → 编译期阻断：`WriteInstruction` 参数类型强制 `taint.SafeString`（`internal/protocol/prompt_builder.go`），`TaintedString` 无法隐式传入
2. zone==ZoneTaintedData 且 Tainted → 接受（正确归宿）；zone!=ZoneTaintedData 且 TaintLevel >= TaintMedium → 降级路由到 ZoneTaintedData + WARN + 审计 `prompt_builder_taint_zone_routing`
3. zone==ZoneMutableSkill → 验证 Ed25519 ApprovalSignature（M9 签发），无效则降级 ZoneTaintedData + WARN + 审计 `prompt_builder_mutable_skill_integrity_failed`
4. 签名通过 → Monotonic Version Gate：`sys_config.min_skill_version`，version < min → 拒绝 + CRITICAL + 审计 `prompt_builder_rollback_attack_blocked`
5. 写入对应 zone string builder

**kernel.PromptBuilder.Build**（非内核调用方；内核四阶段用 `BuildLayered` 五层前缀账本，见 M04，ADR-0105）: 固定顺序 ZoneImmutable → ZoneCoreMemory → ZoneMutableSkill → ZoneTaintedData。ZoneTaintedData（及高污点 ZoneCoreMemory 区块）追加前，对 `[TaintLevel] >= TaintMedium` 内容执行 M11 Spotlighting 包裹：`=== UNTRUSTED_DATA_{sha256(content)[:8]} ===\n{content}\n=== END_UNTRUSTED_DATA ===`；标记由内容 SHA-256 前 8 位派生（非随机）→ 同内容同标记，PromptFn 保纯函数，M12 Eval 回放可验证。M4 上下文注入同此规则。

**SessionResume（崩溃后 ActiveContext 重建）**:
1. 从 M4 FSM Snapshot 恢复 FSM 状态 + DAG 进度
2. 从 [Storage-SQLite] `session_events` 按 AUTOINCREMENT 读 Snapshot 后 Episodic Events
3. 加载 NotesStore 中关联活跃 Note（未过期/未删除）
4. 以 Snapshot WorkingMemorySummary 为基底重放事件 → 重建 ActiveContext
5. 就绪 → Agent FSM 恢复执行
约束: <500ms（Snapshot 后事件 <1000 条）；Snapshot 损坏 → 全量重建；Notes 懒加载（仅 5 条热 Note）；>1000 条事件按 200 条/批增量。

### 2.2 NotesStore

跨 Session 轻量笔记。实现 `internal/memory/store/notes_store.go`（`SQLNotesStore` / `InMemNotesStore`），DDL `internal/protocol/schema/023_notes.sql`。
- 单条 `content` ≤64KB；默认 TTL 7 天（`expires_at = now + 7d`）；系统保留标签（如 `task:{taskID}`）支持任务级聚合（`ListByTask`）
- **写路径**：直接 SQL（`ON CONFLICT DO UPDATE`，CAS `version++`），与 `PromptVersionStore` 同级；CAS 失败静默幂等（调用方可重试）。NotesStore 是 L0 单 Agent 私有状态，不跨 Agent 共享，故不经 MutationBus
- 过期 Note 由 `GC()` 定期清理（直接 DELETE，调用方调度）

### 2.3 用户画像

实现 `internal/agent/context/`（PersonaRefiner + UserProfile），持久化到 `preferences` 表（单条 JSON）。

| UserProfile 维度 | 含义 | 取值示例 |
|------|------|---------|
| `language_pref` | 首选回复语言 | `zh-CN` / `en` / `mixed` |
| `response_style` | 回复风格 | `concise` / `detailed` / `casual` / `formal` |
| `output_format` | 输出格式 | `markdown` / `plain` / `code-first` |
| `expertise` | 领域专业程度（影响解释深度） | `novice` / `intermediate` / `expert` |
| `interaction_summary` | Session 结束时 LLM 生成的用户特征摘要（≤200 字） | 文字 |

**PersonaRefiner**: `Load(ctx)` 启动加载（不存在用默认值）；`Update(signals)` 显式信号即时更新（`/set style concise`）；`RefineAtSessionEnd(ctx, msgs)` 会话结束 LLM 更新 `interaction_summary`（provider 为 nil 跳过）；`Save(ctx)` 与前者配对；`ToUserPreferences()` 转 `[]protocol.UserPreference` 注入 `ImmutableCore.UserPreferences`。
PersonaRefiner 更新**用户偏好画像**；ReflectionMemory 更新 **Agent 自身执行经验**。

### 2.4 核心记忆分页置换（GD-14-002）

`ZoneCoreMemory` 受 `CoreMemoryTotalMaxKB` 硬顶（Tier-0）。`memory_page_out`/`memory_page_in`（`internal/tool/builtin/memory_tools.go` + `memory_tools_exec.go`）允许 Agent 把不再需每轮可见的 Core Memory 块置换到 L2（entity type `PagedCoreMemory`，name=`paged:{sessionID}:{blockKey}`），先 `SemanticMemWriter.UpsertFact` 归档成功才删原块；`page_in` 反向，未页出的 `block_key` 返回 `status=not_found` 软失败（同 `memory_expire`）。

是否调用由 LLM 自主判断：`internal/agent/context/context_pressure.go` 的 `contextPressureHint` 仅把 `TokensUsed/TokenBudget` 比例注入 System Prompt（≥65% 温和提示，≥85% 建议 `memory_page_out`），Go 侧不设强制阈值。与 §10 全局被动回收互补：前者 Agent 主动、任务级、可逆。

---

## 3. L1 Episodic Memory

### 3.1 episodic_events 表

`episodic_events` 是 M2 `events`（[EventLog] 真相源）的派生投影表，DDL 与 `memory_group_mapping` 见 `internal/protocol/schema/003_episodic_memory.sql`。
- **热路径**：`EpisodicMem.Append()` 写 `kv_store` KV 表，Payload 上界 8KB，超限落盘 `~/.polaris/logs/events/<id>.bin` 并替换为 `log_ref`
- **冷投影**：agent 执行层同步向 outbox 写 `target_engine="episodic"`，OutboxWorker `EpisodicProjectorHandler`（`internal/memory/consolidation/episodic_projector.go`）异步 INSERT `episodic_events`，填充 `content`/`salience`/`decay_weight`/`cold` 等

**可变字段白名单**: 仅 `archived`, `decay_weight`, `salience`, `archive_offset` 可受控变更，其余 append-only；每次变更须写 `episodic_events_change_log`（该 log 参与 M11 hash chain）。M2 `events` 仅 INSERT。M11 hash chain 覆盖 `events` 全字段 + `episodic_events_change_log`。

**`[ReasoningState]` 列**（M4 §7.1 跨轮持久化）: `episodic_events.reasoning_state`（nullable）——Provider 返回的推理状态 blob，msgpack + AES-256-GCM 加密（key 由 [CredentialVault].persistent_key 派生）；同 task_id 30min 窗口内 M4 读最近一条注入下次 LLM 调用；SessionPIIVault.SecureZero 同步清零本字段；Tier 0 默认不写（`FeatureReasoningStateCarry` 关闭）。

Salience: LLM 输出 + 工具结果 → 低；用户反馈 + 关键决策 + 失败/成功 → 高；动态调整见 §7.6。

**EpisodicGraphIndexer（GraphRAG 融合）**: `Append()` 写 SQLite 后同步 `EpisodicGraphIndexer.Index(ev)` 在 SurrealDB 图谱建边：`episodic:{id} → TRIGGERED_BY → agent:{agentID}`；`→ IN_SESSION → session:{taskID}`；`→ ACTION_DONE → entity:tool:{tool_name}`（`EventActionDone`，解析 tool_name 动态构边）。实现 `internal/memory/graph/episodic_graph_bridge.go`；best-effort（失败仅日志）；indexer=nil 跳过。使 §7.3 图遍历可从知识实体跨到 episodic 节点（AriGraph arXiv:2407.04363 式混合检索）。

**NamespaceID 分区（GD-14-001，协同任务共享记忆）**：`EpisodicMem.Query` 的 `ev.TaskID != q.SessionID` 过滤是记忆隔离的唯一机制。复用 `types.TaskEntry.Namespace`：CSV Fan-out（`RunCSVFanout`）为同批子任务赋 `Namespace=jobID`；Worker 认领时经 `AgentKernel.SetMemoryNamespace` 注入，`Agent.memoryPartitionKey()` 优先返回 NamespaceID（未设回退 SessionID）作为 Episodic 事件 `TaskID`。仅 4 类协同写入（task_perceived/plan_generated/reflection_completed/execution_completed）参与替换；2PC 幂等 bookkeeping（`agent_execute_dag.go`）与 FastPath 意图缓存仍用 SessionID（防破坏崩溃恢复）。命名空间内共享 ≠ 无限制：TaintLevel 过滤仍生效（`internal/memory/store/episodic_namespace_test.go` `TestEpisodicMem_NamespaceSharing_TaintStillEnforced`）。Blackboard 字段传播见 M8 §11.1。

### 3.2 Session Compaction

Session 关闭时：LLM 生成 3-5 句会话摘要（高 salience 合成事件，`source='compaction'`）；原始事件保留不删；后续检索优先返回合成事件。

### 3.3 Durative Memory

**DurativeMemoryManager（后台聚类引擎）**：扫描近期孤立事件，按时间 + 语义聚类，LLM 判定连续性，建立 DurativeGroup（active/closed/archived）；经 `memory_group_mapping` 关联，不原地修改事件。

Consolidate（每小时 cron）:
1. 扫描 30 天内无 durative_group_id 的孤立事件
2. 按语义相似度 + 时间邻近度聚类
3. LLM 判定候选簇是否语义连续体
4. 创建 DurativeGroup → Append `memory_group_mapping_created` 事件（event_id → group_id）。**禁止原位 UPDATE episodic_events**——[EventLog] 受 M11 AuditTrail 哈希链保护
5. 关闭 >7 天无新事件的 active group

读时 LEFT JOIN `memory_group_mapping` 合成 durative_group_id。

RetrieveWithDurative：检测时间意图关键词（"上周"/"三周前"/"当时"）→ 命中则优先在 group 摘要层搜索，返回组摘要 + top-3 关键事件；未命中走常规检索。

### 3.4 `[ReflectionMemory]` — 元认知反思层

Episodic（"发生了什么"）与 Semantic（"普遍事实"）之间的中间层：Agent 对"我做了什么 + 学到什么"的元认知摘要（对齐 Generative Agents / MemGPT）。

| 层 | 内容示例 | 触发 |
|----|------|------|
| Episodic | "用户问 X, Agent 调 tool_Y, 返回 Z" | Agent 动作即时 |
| **Reflection** | "该类任务上 tool_Y 比 tool_W 快 3 倍；失败模式 X 来自参数 P 越界" | 任务终态 + Session 关闭 + 失败 reflection |
| Semantic | "事实图谱: X is_a Y" | Consolidation 合并 |

**表**（`reflection_memory`，DDL `internal/protocol/schema/024_reflection_memory.sql`，实现 `internal/memory/store/sql_reflection_mem.go`）：`id` PK, `session_id`, `agent_id`, `task_type`（专用列，`idx_reflect_task_type`）, `reflection_type` ∈ {success_pattern, failure_mode, efficiency_insight, cross_task_principle}, `content`（≤500 tokens）, `fail_reason`, `strategy`, `decision`, `salience`（0-1）, `embedding`（float16 BLOB）, `embed_model_ver`, `accessed_count`, `last_accessed_at`（LRU 依据）, `evidence_ids_json`（支持该反思的 Episodic 事件 IDs）, `meta_json`, `created_at`（Unix 秒）。HT0 上限 5000 条（约 5MB），LRU 淘汰批次 100。

**触发**（`ReflectionWorker`，`internal/learning/reflexion/reflection_worker.go`）:
1. 任务终态：S_COMPLETE / S_FAILED 进入且 task_type 在 `ReflectionConfig.TaskTypeWhitelist`（默认 `complex_reasoning/coding/research/debug/analysis`，可经 `NewReflectionWorkerWithConfig` 覆盖）→ 投递 reflection job
2. 失败深度反思：`ReplanCount ≥ ReflectionConfig.MinReplanCount`（默认 2）触发失败模式提取

**写入**：收集同 task_id Evidence Episodic Events → LLM 严格 JSON 提取 → `SQLReflectionMem.AppendReflection` 直写。LLM 提取仅受 CPU/内存空闲约束（M9 BackgroundTaskScheduler [Priority-2]）。

**读取**：HybridRetriever 第 4 路（权重 0.15）：`reflectionMem` 非 nil 走 `QueryReflections(Topic=query)` SQL 索引查询，nil 降级 KV 前缀 `reflection:` 扫描。S_PLAN（ADR-0105 决策四）：`BuildPlanContext` 用本回合 `TaskModel.Goal` 调 `ListReflections(Topic=Goal, K=3)`，与情景/L2/RAG 按 `m4_kernel.recall.*` 预算渲染写入 L4 数据区；Perceive 不查反思（TaskModel 为上一回合遗留），回合内经 `StateContext.TurnRecall` 复用。反思为 Agent 自生成（TaintLow），不走 PromptBuilder 写入门控、不入 ZoneImmutable。与 [HeuristicsMemory]（M9 §2.1，task_type→prompt 模板）互补。

---

## 4. L2 Semantic Memory

### 4.1 表结构

DDL `internal/protocol/schema/004_semantic_memory.sql`（[Storage-SQLite] 邻接表）；图存储 [Storage-SurrealDB-Core]。

### 4.2 UpsertFact

1. searchSimilar(阈值 0.95) → 同一事实，UPDATE 属性 + version++
2. 相似度 > 0.80 → LLM 冲突解决（更新 vs 新事实）
3. 相似度 < 0.80 → INSERT 新事实
4. version++ 不可变版本 + source_event_id provenance + 信念修正（矛盾时优先更近期/更高证据强度）+ Prospective Indexing（写入时预生成未来查询并索引）

**[Entity 生命周期]** `semantic_entities.status` ∈ `active`/`superseded`/`expired`/`merged`（默认 `active`）+ `superseded_by` 外键。被取代旧事实置 `superseded` 不物理删除；检索过滤 `WHERE status='active'`，保留完整信念修订轨迹。
- **精确名称冲突**：ON CONFLICT UPDATE 更新属性，同时 `MarkEntitySuperseded(oldDBID, 0)` 置旧行 `superseded`（直接 SQL，同步，非 MutationBus）
- **Jaccard 近重复检测**（仅 `user_preference`）：`ListActiveEntities` 取同类活跃实体，名称分词 Jaccard `> 0.6` 视为矛盾旧观念，`MarkEntitySuperseded` 后再 INSERT

**[接口约束]** SemanticMemory 事实/关系写入方法声明于 `internal/protocol/interfaces_memory.go`，实现 `internal/memory/`；写入必经 MutationBus，禁止绕过 M2 单写者直接 SQL。实体生命周期管理（标记废弃、列举活跃、UserProfile 读写）属轻量同步读写，走直接 SQL。Embedding 存 BLOB（float32→float16 量化，工具在 `internal/llm/`）。

**[XR-16 读写对称]** `taint_level` 写路径 only-up（ON CONFLICT `MAX(taint_level, excluded.taint_level)`）；`GetEntity` 读路径 SELECT 含 `COALESCE(taint_level, 0)` 并绑定 `ent.TaintLevel`（ADR-0025 BUG-4）。绕过此绑定的直读视为 XR-16 违规。

### 4.3 QueryClassifier + RetrievalRouter

实现 `internal/memory/retrieval/query_classifier.go`。
- **Tier-0**：纯规则中文关键词（<10µs，无 LLM/embedding）：时间词（"最近"/"上周"/"历史"）→ `temporal`；操作词（"如何"/"怎么做"/"步骤"）→ `how_to`；事实词（"是什么"/"定义"/"谁是"）→ `factual`；推理词（"为什么"/"分析"/"比较"）→ `reasoning`；未命中 → `unknown`（等效全搜）
- **Tier-1+**：`ClassifyQuerySemantic(ctx, query, embedder)`（`query_classifier_semantic.go`）；`InitPrototypes` 预计算 4 类原型向量并缓存，余弦比较，置信度 <0.3 回退 `unknown`；embedder=nil 或原型未初始化降级 Tier-0
- **RetrievalRouter**（在 `HybridRetriever.Search` 内）：`temporal` → 激活第 5 路 `DurativeMemoryManager.RetrieveGroups`；其余 → BM25 + Simhash + Graph + ReflectionMem 融合。RRF 权重见 §7.4

---

## 5. L3 Procedural Memory

三层存储：SurrealKV KV 热路径签名级精确查找（skill_id → Wasm 二进制，<10µs）；SurrealDB-Core 语义搜索（System 2 embedding 相似技能检索）；文件系统 SKILL.md 为 Ground Truth（受 Git 版本控制）。

双轨检索：System 1 经 IntentSignature 在 SurrealKV O(1) 精确匹配，命中后由 M6 WasmSkillCache 缓存编译产物直接执行；System 2 在 SurrealDB-Core KNN 语义搜索，候选按成功率排序，渐进披露注入 prompt。

L3 索引相关 DDL 托管于 M2 SurrealKV 引擎（`internal/store/surreal_store.go`），SKILL.md 元数据懒加载。M5 skillKV 与 M6 WasmSkillCache 关系见 M6 §5.1。

---

## 5-bis. LLM 主动写记忆工具（Agent Self-Memory Writing）

与 §6 被动路径（轮次结束后异步积累）并存：LLM 在推理中主动调用工具即时写入/更新语义记忆，同轮可确认已持久化。

### 5-bis.1 设计动机

被动积累仅在对话结束后异步落盘，LLM 当轮无法确认事实已持久化；主动写工具同步返回写入结果，后续对话可立即检索。

### 5-bis.2 工具集（`internal/tool/builtin/memory_tools.go`）

| 工具名 | Capability | 语义 |
|--------|------------|------|
| `memory_write` | `CapWriteLocal` | 写入/覆盖语义事实（`SemanticMemWriter.UpsertFact`），支持 `valid_until` |
| `memory_search` | `CapReadOnly` | 混合检索（BM25 + vector + graph），支持 `as_of` 时空穿梭 |
| `memory_append` | `CapWriteLocal` | 追加属性到已有实体（upsert，不覆盖 description） |
| `memory_expire` | `CapWriteLocal` | 标记实体失效（`MarkEntityExpired` → `status='expired'`），含 reason 审计 |
| `memory_page_out` / `memory_page_in` | `CapWriteLocal` | Core Memory 块换出/换回（§2.4，`protocol.CoreMemory`） |
| `memory_reflect` | `CapWriteLocal` | 记录反思/洞察/长期决策到 ReflectionMemory |
| `core_memory_edit` | `CapWriteLocal` | 编辑 Core Memory 块（ADR-0033，`core_memory_edit.go`） |

共 8 个工具，均 `SandboxTier = SandboxInProcess`、`RiskLevel = RiskLow`，经 PolicyGate 五阶段后在 InProcessSandbox 执行。

### 5-bis.3 注册路径

`boot_tools.go`（或 `boot_agent.go`）调用 `builtin.RegisterMemoryTools(sbx, toolReg, exclusiveWriter, semanticWriter, retriever, reflection, coreMemory)`：`sbx.Register` 注册执行函数，`toolReg.Register` 注册元数据。
- `semanticWriter`：`SemanticMemWriter`，实现 `internal/memory/store/semantic_mem.go`
- `retriever`：`protocol.HybridRetriever`，实现 `internal/memory/retrieval/retriever.go`
- `exclusiveWriter`：`retrieval.ExclusiveWriter`，写工具串行化入口
- `reflection`：`protocol.ReflectionMemory`；`coreMemory`：`protocol.CoreMemory`
- 工具元数据内联构造（不走 `tool.LoadBuiltinToolMeta` embed FS），防 `builtin/<name>/` 缺失时静默跳过

### 5-bis.4 与被动写路径的关系

| 维度 | 被动路径（§6） | 主动写工具（§5-bis） |
|------|--------------|-------------------|
| 触发 | Agent 轮次结束后 outbox 异步 | LLM 推理中 tool_call |
| 目标 | 情景记忆 → 冷投影 → 语义 | 直接写语义记忆 |
| LLM 可确认 | 否 | 是（同步返回） |
| 数据来源 | 系统自动采集轨迹 | LLM 显式决策 |

---

## 6. Write Path: Hot/Cold 分离

- **热路径 1（同步）**：`EpisodicMem.Append()` 序列化写 `kv_store`，Payload 超 8KB 截断落盘；同步更新 Working Memory 缓存，触发 TokenBurnRate 计数。不调 LLM、不操作图、不等 embedding API
- **热路径 2（outbox）**：写 kv_store 后同步向 outbox 投递 `target_engine="episodic"`
- **冷路径（OutboxWorker 异步）**：`EpisodicProjectorHandler` 消费 → INSERT `episodic_events`；`OnlineReindexer` 扫描 `embed_model_version=''` AND `cold=0` 的行调 M1 Embedding API 填充向量；其余 handler 做图写入、实体/关系 Upsert、Consolidation 阈值检查、Skill 成功率统计。`cold=1` 的历史事件跳过 embedding

---

## 7. Read Path: HybridRetriever

实现 `internal/memory/retrieval/retriever.go`（HybridRetrieverImpl）。持有 store / graph / durative / reflectionMem / embedder 五个可选路径字段，多个构造器按激活路径数递增（BM25+Simhash → 含 Graph+Durative+ReflectionMem 全路径），向量路径可构造后注入。6 路融合：BM25 + Simhash + Vector + Graph + ReflectionMem + DurativeMemory，RRF（k=60），权重 1.0/0.8/0.6/0.6/0.15/0.3。

### 7.1 BM25Index

- 存储 [Storage-SurrealDB-Core] BM25（k1=1.2, b=0.75）；Tokenizer：jieba-rs / lindera 多语言，M3 启动期按 locale 选默认
- Search：分词 → BM25 分数降序 topK → 批量加载完整事件
- 冷路径异步 Index(event)：Unicode 标准化 + 词干提取 + 停用词过滤 → 倒排索引

### 7.2 VectorIndex + Simhash

- 存储 [Storage-SurrealDB-Core] HNSW，维度由 M1 `Embedder.Dimension()` 运行时决定（M2 §2.5）；Search：`embedder.Embed(query)` → KNN 余弦距离 → topK
- **Simhash 备选**：64-bit 指纹（纯 Go, <10µs/text），embedding API 不可用时汉明距离 ≤8（`internal/memory/util/simhash.go`）替代 DenseVec 路径，BM25 + Simhash + GraphTraverse 三路融合。廉价近似，非替代

### 7.3 GraphTraverser + Spreading Activation

Traverse（BFS）：种子 = semantic_entities 中与 query embedding 最相似 top-5 → 有界 BFS（depth=2），entity_type 加权 Person ×2.0 / Project ×1.5 / Tool ×1.0 → 去重按路径权重和降序；限制 maxNeighborsPerHop=20, maxTotalNodes=200。

Spreading Activation（关联发现）：top-3 种子 activation_energy=1.0 → energy × edge.weight 传播至邻居，自身 ×0.7 衰减（`energyDecay=0.7`），energy ≤0.05 停止（dormancyThreshold）→ 最多 5 轮（fanOutLimit=5）→ 按最终 energy 降序 topK。模式选择：query 含"为什么"/"原因"/"关联"/"影响" → Spreading Activation，否则 BFS。

### 7.4 RRF 融合 + BM25 精排

**6 路召回**（scope=memory）：
- BM25（1.0）：KV 前缀扫描 + TF×IDF 近似，Tier 0 纯 Go
- Simhash（0.8）：64-bit 指纹，sim=1-(dist/64)
- Vector（0.6）：KNN 余弦，embedder==nil 跳过
- Graph（0.6，Tier1+，graph==nil 跳过）：BM25 Top1 作起点图遍历，跳数衰减赋分
- ReflectionMemory（0.15）：见 §3.4
- DurativeMemory（0.3，ClassifyQuery==Temporal 激活）：RetrieveGroups(5) 摘要参与 BM25 打分

**RRF**：`score(d) = Σ weight_i / (k + rank_i + 1)`，k=60（`spec/state.yaml §m5_memory.rrf_k`）；截断 FinalTopK=20（config.FinalTopK 可覆盖）。

**污点传播**：RRF 聚合阶段维护 taintMap（只升不降），ScoredFragment.TaintLevel 按来源前缀赋值：episodic/durative_group → TaintHigh，其余 → TaintMedium（inv_M5_06）。调用方注入 Prompt 前须遵循 PropagateTaint。隐私门控由上层 M11 Policy Gate 在 ctx 注入，retriever 不内联 ACL。

**M5/M10 共享**：检索范围不同（M5：episodic+semantic 前缀；M10：doc_nodes/chunk:），`HybridRetrieverImpl` 经 `scope.Type` 路由，一套实例；M10 参数差异见 M10 §2.2。

**跨版本嵌入兼容**：`MemoryEntry.EmbedModelVersion` 触发 `OnlineReindexer`（inv_M5_03）；Tier 0 BM25+Simhash 路径不依赖 embedding API，模型变更不影响 Tier 0 召回。Tier0 降级向量路（`fetchVectorResultsFromSQL`，SQLite 余弦扫描）按版本过滤（ADR-0113 决策四）：版本取自 `hr.embedder.ModelVersion()`，SQL 为 `embed_model_version = ?` 参数化过滤；embedder 为 nil 或版本为 `''`（未就绪）不走向量路；未重嵌的旧版本事件在 OnlineReindexer 收敛前只经 BM25 召回。查询失败 Warn 日志后降级返回空（ADR-0087）。换模型清空向量库时版本戳同步置空（ADR-0113 决策六）。

**OnlineReindexer**（`internal/memory/retrieval/online_reindexer.go`）：
- 触发：`episodic_events.embed_model_version = ''`（尚未投影）或 `!= currentVersion`（模型切换），走 `idx_ep_embed_ver` 偏索引
- 接口：consumer-side `Embedder`（`Embed(ctx, text) ([]float32, error)` + `ModelVersion() string`），M1 EmbeddingBatcher 实现并注入（防包循环）
- 批处理：`Run(ctx)` 每次 50 条（`defaultReindexBatchSize`），每条后 `runtime.Gosched()`，单条失败不中断整批
- 量化：float32 → float16 BLOB（IEEE 754 half，小端），与 DDL 003/004 一致
- 返回 `(processed int, remaining bool, err error)`；调用方循环至 `remaining=false`；版本切换由调用方决策重触发，避免无限循环

### 7.5 Temporal Retrieval (AsOf)

提供 `as_of`（非 0）时，语义记忆实体命中解析阶段检查 `valid_from`/`valid_until`；`as_of` 时点未生效或已过期的事实被过滤，不影响当前活动状态。

### 7.6 Evidence Subgraph Extraction

`EvidenceSubgraphExtractor`（设计位置 `internal/memory/graph/edge_weight.go`）**尚未实现**（仅注释提及）：设计为种子实体有界 BFS（maxDepth=2, maxNodes=50）+ alpha 权重过滤（PPR 近似剪枝）；邻接表 key `adj:<entityID>`，边权重 key `edge_w:<src>_<dst>`。

### 7.6 Edge Weight Reinforcement & Decay

`EdgeWeightManager`（`internal/memory/graph/edge_weight.go`）：`DecayUnused` 读时 O(1) 指数衰减（防写放大）；`ReinforcePath` 强化路径；`FeedbackCalibrate` 成功轨迹每条边 +0.03（上限 1.0），持久化至 KV `edge_w:<edgeID>`；`PeriodicPrune` 扫描 `edge_w:` 前缀批量删除 weight < pruneThreshold 的边。

---

## 8. Effective Connectivity（冷后台预计算）

`semantic_connectivity_cache` DDL 见 `004_semantic_memory.sql`（派生缓存，非事实源）。`ConnectivityPrecomputer` **尚未实现**（仅 DDL 与设计）：设计由 M9 BackgroundTaskScheduler 每日 4:30 触发（与 Consolidation 3:00 错开 ≥90 分钟）；Tier 0 最多 200 个种子实体（~20MB），Tier 1+ 1000 个；50 实体/批，批间 Gosched，CPU >30% 或空闲内存 <2GB 挂起；INSERT OR REPLACE 全量覆盖。查询时 ActivationMaximization O(1)：任务 embedding 搜最相似 20 实体 → 按 effective_weight 排序取 topK + BFS+PPR 构建最小激活子图。

---

## 9. Consolidation

**触发**：主题转换 shift → 立即；eventCount ≥ 50 → 触发并归零；sessionClosed → 强制。

**5-Stage Pipeline**（`internal/memory/consolidation/`）:

- **Stage 1 — 实体/关系提取**：
  - 主路径 `SharedEntityExtractor`（复用 M10 `graphrag.GraphBuildPipeline` Phase1/Phase2，消除重复 LLM 燃烧与表述漂移，ADR-0083 决策，原 0077）；不可用时聚合 Session 内 Episodic 文本独立调 LLM，提取实体（`user_preference`, `constraint`, `temporary_conclusion`, `entity`）与关系（`depends_on`, `configures`, `conflicts_with`, `relates_to`, `derived_from`），结构化 JSON；LLM 也不可用降级正则。`derived_from`（GD-14-001）标注"实体 A 是实体 B 的推论/派生"，区别于运行时/配置依赖 `depends_on`，供级联失效识别派生血缘
  - 写入期去重桥接（ADR-0083）：经 `SharedEntityExtractor` 的实体标 `source_type='graphrag_ingest'`，与 M10 共享去重键；检索期 Spreading Activation 以此联合两侧种子（M10 §2.7）
- **Stage 2 — Upsert Semantic + 生命周期 + 级联失效**：
  - 批量 `UpsertFact / UpsertRelation`，经 `retrieval.ExclusiveWriter` 闭合旧事实；精确名称冲突与 Jaccard（>0.6，`user_preference`）命中的旧实体均 `MarkEntitySuperseded`，**两分支共用同一级联失效触发路径**；单条失败不中止批次
  - **`[CascadeInvalidator]`**（`internal/memory/retrieval/cascade_invalidator.go`）：`ExclusiveWriter` 闭合旧事实后触发，沿 `semantic_relations`（全部 relation_type，含 `derived_from`）用单条递归 CTE（`WITH RECURSIVE`）自被取代实体扩散，最多 2 跳（`maxCascadeHops`），命中的相邻实体批量置 `status='pending_review'` 并写 `episodic_events_change_log` 审计，交后续人工/自动复核，不直接删除或改写。CTE 用 path 列防双向边折返，hop 列保证有环图有界终止
- **Stage 3 — 会话摘要**：LLM 3-5 句（source='compaction'）写 `SemanticMemory.StoreDocument`；LLM 不可用降级为事件类型频次拼接
- **Stage 3.5 — UserProfile 合成（L3 Persona）**：`len(events) >= 10` 触发；LLM 路径（512 token）提取 `stable_facts`/`recent_activity`/`behavioral_patterns`，不可用降级规则路径（工具频次 + 事件类型统计）；写 `user_profile` 表；非阻塞，失败静默跳过
- **Stage 4 — Logic Collapse → Skill Library**：统计 Session 内同名工具成功调用次数，≥3 次注册为 `SkillMeta`（`SkillRegistry.Register`）；SkillRegistry 为 nil 跳过

> 约束：version++ 不可变版本 + source_event_id provenance + 信念修正 + 单条失败不中止整体管线。

**依赖注入**：`NewConsolidationPipeline(episodic, semantic, skills, summarizer)`——episodic/semantic 必须非 nil，skills/summarizer 可选（nil 则对应阶段降级或跳过）；`NewConsolidationPipelineFull` 额外注入 `writeFilter`、`cascadeInv *retrieval.CascadeInvalidator`、`db`。summarizer 为 `memory.LLMSummarizer` 接口（`Summarize` + `InferRaw`），Stage 1 与 Stage 3.5 经 `internal/prompt/templates` 渲染模板 + `InferRaw` 调用。

**超时保护**：`Run` 在调用方未设 deadline 时自动注入 5 分钟超时，防 LLM 长调用阻塞 M9 调度器。**冷启动 IO 锁**：`EpisodicMem.Query` 冷启动 `store.Scan` 在锁外执行，锁内仅合并结果。

---

## 10. Forgetting: 双层策略

### 10.1 热删除：效用衰减

效用指数衰减：`salience × exp(-decayRate × ageHours/24)`，decayRate = 0.01/日。低于 salienceThreshold（默认 0.15）写 tombstone（key=`forgettable:{id}`），不物理删原事件。salienceThreshold 为静态配置阈值，不随任务熵动态调整（Q-Learning 熵门控 `QLearner` 已于 ADR-0062 清理中删除）。Forgettable 事件原地保留，由 `ColdArchiver.PhysicalCompact` 最终清理。

### 10.2 冷归档

Forgettable 且年龄 >30 天的事件移入归档前缀（`archive:episodic:{id}`），原 key 与 tombstone 同时删除；`StoreCapabilities.SupportsSQL=true` 的引擎触发 VACUUM 回收磁盘；归档数据按需前缀扫描回查，无删除限制。`ColdArchiver` 在 `internal/memory/consolidation/consolidation_archive.go`；与 M2 §3.4 `EventArchiver` 职责不同。

`PeriodicCleanup` 扫描优化：KV 层支持原生 SQL（`dbAccessor` 断言）时直接 `SELECT id, salience, occurred_at FROM events WHERE topic IN ('memory.openclaw','memory')`，避免全表遍历与 ULID 解析。`PhysicalCompact` 扫描 `forgettable:` 前缀批量清理——两步分离保证单次扫描不阻塞写路径。

---

## 11. Context Assembler / PromptBuilder

> ADR-0105 决策十：原 `Assembler` 第二条召回管线已删除，召回只在回合内（`agent/context/recall*.go`）经 RRF 融合后注入 L4。L2 语义召回由 `cmd/polaris/adapters_recall.go` 的 `recallCognitiveAdapter` 接线（仅 `sement_` 语义实体，FTS 命中后按 ID 取正文，项目隔离经 `projectScopedFTS`）；情景事件经 `FTSEpisodic`（BM25，`projectScopedEpisodicFTS`），`EpisodicQuery` 以 `IDs` 取代已删除的 `Semantic` 子串匹配；用户画像只走 L0 稳定核，召回不含画像段。详见 M04 召回段。

PromptBuilder 实现 `internal/agent/`，SessionCompressor 实现 `internal/memory/`。

### 11.0 系统提示词三层组装

稳定层由 `ImmutableCore.StableMessage()` 渲染（L0，内核路径），易变层由 `ImmutableCore.VolatileContent()` 取出置于对话历史之后（L3）；`PrependToMessages()` 仅供网关直连等非内核调用方。稳定层内集合（工具名、插件/MCP 名称、用户画像）必须确定序，MCP 连接状态标记属易变量、只进易变层（ADR-0105 决策一）。三层对应 KV Cache 由稳定到易变排列：

| 层 | 内容 | 变更频率 | 位置 |
|----|------|---------|------|
| stable | Agent 身份（SOUL.md 或 `DefaultPolarisIdentity`）+ 模型感知工具调用引导 + 平台感知提示 | 跨会话不变 | `internal/memory/` |
| context | 项目上下文（M13 注入 ambient skills / 全局目标） | 会话内不变 | `internal/gateway/` |
| volatile | 当前日期（精确到天，避免逐分钟破坏 prefix cache） | 每天 | `ImmutableCore.VolatileBlock` |

- **用户自定义身份**：`~/.polaris/config/SOUL.md` 存在时覆盖 `DefaultPolarisIdentity`，启动时读取并缓存到 `Server.soulMDContent`
- **模型感知引导**：`memory.NeedsToolUseEnforcement(modelID)` 判断模型族，注入 `memory.ModelSpecificGuidance(modelID)`，防模型仅描述意图而不调用工具
- **平台感知提示**：按接入平台（cli/webui/api/cron）注入输出格式说明（`internal/memory/`），`POLARIS_PLATFORM` 控制，默认 `webui`
- **M9 激活路径**：`PromptVersionStore.OnActivate` 在 `task_type='general'` 版本激活后热更新 `Server.activatedSystemPrompt`（受 `activatedSystemPromptMu` 保护），下一轮 `injectSystemPrompt()` 生效。`SystemPromptTemplate` 非空时全量走模板渲染、跳过三层组装

### 11.1 三所有权层

| 层 | 存储位置 | Owner | 变更方式 | DB re-init | Factory Reset |
|----|---------|-------|---------|-----------|--------------|
| **Layer 0** 内置默认 | `configs/prompts/*.md`（go:embed） | Polaris 项目 | PR + 重新编译 | 不受影响 | 不受影响 |
| **Layer 1** 用户自定义 | `~/.polaris/config/prompts/*.md` | 用户 | 直接编辑 或 `PUT /v1/config/prompts/{name}` | **存活**（文件不在 DB） | `DELETE /v1/config/prompts/{name}` 显式删除 |
| **Layer 2** M9 优化 | DB `prompt_versions` | 自进化引擎 | M9 自动生成 + Staging 审批 | **重置** | 随 DB 删除 |

Layer 1 白名单（API `GET/PUT/DELETE /v1/config/prompts/{name}`，实现 `internal/gateway/`）：`identity`（整段替换 Layer 0 identity.md）、`custom_instructions`（拼接到身份之后）。不暴露：tool_enforcement、platform hints。re-init 只清 M9 数据，`DELETE` 才清用户自定义。

**Prompt 组装顺序与 KV Cache**：块按稳定度从长到短排列，最久不变者最前：`ImmutableCore → Procedural (Skills) → Semantic (Knowledge) → Episodic (Recent Events) → Working (Scratchpad) / TaintedData`。M1 Provider Adapter 自动探测该顺序，向稳定块末尾注入 `cache_control: {"type":"ephemeral"}`（M1 §3）。Working/TaintedData 置最后，不参与 Cache。Layout Zone → ContextZone 映射见 §2.1。

### 11.3 SessionCompressor

> 编号说明：本节曾丢失编号，而全仓 17 处注释一致以 `M05 §11.3` 引用，故补回 `11.3`。§11.2 空缺为历史删除留白，**不得**改号为 11.2。

实现 `internal/memory/`。与 M4 ContextWindowManager 协同：M4 §7 持有热路径阈值（>70% salience 排序候选 / >90% 语义结构感知逐出），M5 在 M4 调用时执行冷压缩（不独立设阈值）：
- **Stage 1**：tool output pre-pruning——超 10KB 的 `tool_result` 替换为存根 `[offloaded: N bytes → read_tool_ref("xxx")]`，原内容经 `ToolRefOffloader` 落盘，可按 node_id 回取
- **Stage 2**：LLM 锚点摘要——以 currentSummary 为锚追加新事件产生增量摘要（`internal/memory/compact` 的 `Summarize`，M4 调用后写回锚点）
- **Stage 3**：任务画布注入——将 `RenderTaskCanvas(ctx, sessionID)` 输出（`graph LR`）前置注入 anchor：`## Task State (node_id → read_tool_ref)\n{mermaid}\n## Summary\n{anchor}`；画布为空跳过（TencentDB Agent Memory 符号化短期记忆，~61% token 节省）

**任务画布**（渲染 `internal/memory/graph/mmd_canvas.go`，入口 `MemImpl.RenderTaskCanvas(ctx, sessionID)`）：自该会话在 `session_trajectory`（045）的工具行（tool_name/tool_ok/payload）纯函数渲染 Mermaid `graph LR`，相邻步骤顺序连边；节点上限 30（最近 30 步），~8 token/节点。读取端由 cmd/polaris 注入（`SetTrajectoryReader`），无会话 ID/无工具调用/读取失败均返回空串（ADR-0104 决策七：原全进程共享单例 `TaskMermaidCanvas` 已删除）。

锚定策略：架构决策、失败原因、修复方案、用户风格偏好永久保留；当前进度和待办允许更新；具体工具输出允许丢弃。

---

## 12. Embedding Drift 对策

### 12.1 维度切换无缝过渡

M1 Embedder 维度变化（或切 local_only）时：M5 检测当前 Provider 维度，动态将查询路由至 SurrealDB-Core 对应维度的隔离表（如 index_remote_4096 / index_local_384）；维持 DenseVec 默认权重，禁止因维度不兼容退化为纯 BM25；M6 同理经独立维度表维系 L1 vecIndex 权重。

### 12.2 Blue-Green Index Swap

Tier 0 前提：空闲内存 > 2 × 当前向量索引占用（M3 sysinfo.FreeMemory()）；不满足退化为原地增量更新。
1. 后台构建新版本索引 index_v{N+1}，查询仍用 index_v{N}
2. 质量验证：锚定样本(100 条) Recall@5 ≤ 旧索引 90% → ABORT + WARN
3. 原子切换索引版本指针 (<1ms)
4. 异步回收 index_v{N}
5. FTS5/BM25 始终在线

### 12.3 DriftDetector

实现与编排见 `internal/learning/surprise/drift_orchestrator.go`；配置见 `internal/config/thresholds.go`。
- DriftDetector：anchors(100 AnchorSample)，checkInterval(7d)，driftThreshold(0.05)。Detect：sampleCount<5 的任务类型跳过（unknownCount++）→ 重新检索算 Top-5 变化率 + 余弦距离变化 → changeRate>0.4 且 cosineDelta>driftThreshold 记漂移 → unknownRatio>0.30 系统级告警
- EmbeddingVersionTracker：每索引维护 P50/P95/P99/Min/Max 滚动统计（EWMA α=0.01）；跨版本检索 min-max 归一化 → RRF 融合

---

## 14. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| HybridRetriever 单路失败 | 其余路权重接管（DenseVec 失败 → BM25×0.7 + Graph×0.3） | 故障路恢复后切回默认权重 |
| Embedding API 不可用 | Simhash（汉明距离 ≤8）+ BM25 + 图遍历三路融合 | API 恢复后 DenseVec 权重切回 |
| SurrealDB-Core 维度切换 | 动态路由至对应维度表，无需降级 BM25 | 后台静默回填增量 |
| Consolidation LLM 超时 | 跳过本轮，事件保留等待下一轮 | 下个 cron 周期重试 |
| NotesStore CAS 冲突超限（>3 次） | 写入 notes_conflict_log shadow 表供人工裁决 | — |
| Episodic 冷路径 Outbox 积压 | 暂停非关键 Consolidation + WARN | 积压 <200 恢复 |
| Mem-L3 SurrealKV 故障 | 降级 SQLite 备份索引（skill_id→metadata，不含 blob） | SurrealKV 恢复后切回 |
| DurativeMemory 聚类 LLM 超时 | 按纯向量余弦聚类（跳过 LLM 语义判定） | LLM 恢复后追加语义连续性标注 |
| Context 组装超时 (>500ms) | 跳过 ZoneMutableSkill，仅发 ZoneImmutable + 摘要 | 下次 refresh 完整组装 |
| EdgeWeightManager 修剪 LLM 超时 | 仅物理 DELETE < pruneThreshold 的边，跳过 FeedbackCalibrate | 下个 cron 周期校准 |
| DriftDetector 检测到漂移 | 该 task_type 降级纯 BM25，其余不受影响 | Blue-Green 重嵌完成后切回 |

与 OSMemoryGuard 协同：L1 预警 → 暂停 Consolidation 冷路径；L2 紧急 → 暂停 Episodic 冷路径 Outbox、限制 WorkingMemory 容量；L3 临界 → 全部冷路径暂停，仅热路径写入 + L0 读取可用。

---

## 默认参数

阈值与重评触发条件: `spec/state.yaml §thresholds.m5_memory`。

## 15. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M1 Inference | Embedding API（向量生成）、LLM 调用（摘要/Consolidation） | M1 §6.1, §5 |
| M2 Storage | Store 接口、EventLog 真相源（events → episodic_events 派生投影） | M2 §1.1, §2.1 |
| M4 Agent Kernel | PromptBuilder（三 Zone 组装 + Spotlighting 门控）、HybridRetriever 上下文检索 | M4 §2, §10 |
| M6 Skill Library | L3 Procedural Memory 技能索引 + SurrealKV 缓存 | M6 §7 |
| M9 Self-Improve | M9→ZoneMutableSkill Taint Gate（双层）、Preference Learner、PersonaRefiner | M9 §1.1, M5 §2.1 |
| M10 Knowledge RAG | HybridRetriever 共享引擎（`internal/store/`）、检索配置差异 | M10 §2.2 |
| M11 Policy Safety | SafetyRules 注入 ImmutableCore、TaintGate 写入 Zone 校验 | M11 §2, M5 §2.1 |
| 全局字典 | HE-Rule-4、HybridRetriever/RRF 定义 | 00-Global-Dictionary §2, §9-bis |
| DDL | 001_events（真相源）、003_episodic_memory（派生投影）、004_semantic_memory（语义层） | internal/protocol/schema/001-004_*.sql |

---

## 16. 实现状态与 2026 研究对照

### 实现记录

全部已完成并在生产路径接线：L2 SemanticMem（MutationBus 写路径）；Entity 生命周期（status/superseded_by + Jaccard 0.6）；UserProfile 合成（`user_profile` 表，Stage 3.5）；任务画布（`internal/memory/graph/`，按会话从轨迹表渲染）；EpisodicGraphBridge；ReflectionMem（HT0 LRU）；DurativeMemoryManager；EdgeWeightManager；ReflectionWorker（`internal/learning/reflexion/`）；CascadeInvalidator（GD-14-001，`internal/memory/retrieval/cascade_invalidator.go`）。
**未实现**：EvidenceSubgraphExtractor（§7.6）、ConnectivityPrecomputer（§8）、SynapticPlasticityManager（仅 DDL 注释提及）。

### 引入计划（优先级排序）

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **D-MEM 多巴胺门控巩固** | arXiv:2603.14597 | 以 SurpriseIndex 作门控，仅 surprise > 阈值的情节才晋升语义层，消除冗余写入与 O(N²) 延迟 | `internal/memory/` + `internal/learning/` | P1 |
| **Path-Constrained Retrieval** | arXiv:2511.18313 | BFS 限定关系类型白名单（uses/depends_on/extends），防多跳语义漂移 | `internal/store/`（HybridRetriever GraphTraverse） | P2 |
| **E-mem 多 Agent 情节重建** | arXiv:2601.21714 | 异构辅助 Agent 维护未压缩情节上下文，token -70%，F1 +54%；单节点情节记忆在 swarm 场景是盲点 | `internal/execute/orchestrator/`（中期） | P3 |


## 13. MemoryAgent (Swarm Integration)

`MemoryAgent`（`internal/swarm/agents/memory_agent.go`）是 swarm 内的后台"记忆看护"worker：
1. **Contextual Whispers**：周期扫描 `episodic_events` 中近期未归档的高 salience（`>=0.7`）事件，与当前活跃目标/blackboard 任务相关者经 `whisperChan`（`MemoryWhisper`）推给活跃 Agent；高水位游标存于统一游标表 `consumer_cursors`（consumer_id=`memory_agent.whisper`）
2. **Graph Maintenance**：周期调用 `mem.PruneMemoryGraph`（EdgeWeightManager 衰减未用边、修剪低于阈值关系）
3. **Memory Pressure Relief**：系统极端内存压力（`memPressure`）时节流

MemoryAgent **不再**做 L1→L2 蒸馏；全部蒸馏由 outbox 事件触发的 `ConsolidationPipeline` 承担。
