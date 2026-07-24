# 模块 5: Memory System

> 四层记忆（Working / Episodic / Semantic / Procedural），多存储引擎绑定，[Tier-0-Limit]
> Go（记忆管理器 + 检索路由 + Consolidation），Rust（Embedding 计算 via M1）
> [HE-Rule-4] [HE-Rule-5] [HE-Rule-6]
<!-- §跳读: 0-bis:7 职责 / 0-ter:19 不变量速查 / 1:30 四层映射 / 2:41 L0 Working / 3:125 L1 Episodic / 4:227 L2 Semantic / 5:269 L3 Procedural / 6:323 写路径 / 7:335 HybridRetriever / 8:421 EffConn / 9:431 Consolidation / 10:466 Forgetting / 11:483 PromptBuilder / 12:553 Drift / 14:591 496(SOFT)降级 / 15:615 依赖 -->
## 0-bis. 职责边界

- M5 **是**: 四层记忆（Working/Episodic/Semantic/Procedural）的读写管理器 | M5 **不是**: 记忆的物理存储引擎（那是 M2）
- M5 **是**: HybridRetriever 检索路由（BM25 + Dense + Graph → RRF） | M5 **不是**: Embedding 向量计算（那是 M1 Embedding API（Application Programming Interface，应用程序接口））
- M5 **是**: Consolidation 语义压缩（episodic→semantic） | M5 **不是**: Skill 技能执行（那是 M6）
- M5 **是**: ImmutableCore 用户长期偏好管理（永不裁剪）+ UserProfile 自动合成（L3 Persona，§9.5） | M5 **不是**: 安全策略决策（那是 M11）
- M5 **是**: Context 上下文管理与组装（由 `kernel.PromptBuilder` 承担 Slot 分离 + Taint 门控）+ TaskMermaidCanvas 符号化压缩（§11.3） | M5 **不是**: Agent 任务调度（那是 M4）
- M5 **是**: Forgetting 策略（效用衰减 + 冷归档） | M5 **不是**: 物理数据删除（委托 M2 Storage）
- M5 **是**: Entity 生命周期治理（active/superseded/expired）+ Jaccard 矛盾检测（§4.2）| M5 **不是**: 策略执行（那是 M11）

---

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M5_01 | ImmutableCore 永不参与压缩——ContextWindow.Compress 跳过此区域 | CI（Continuous Integration，持续集成） `immutable_core_integrity` 测试 |
| inv_M5_02 | ImmutableCore 写入必经 staging 审批——`provenance_id` CHECK 约束 | DDL（Data Definition Language，数据定义语言） CHECK + M11 审计 |
| inv_M5_03 | embed_model_version 是一等字段——每 chunk/event 携带，跨版本检索走 OnlineReindexer | DDL NOT NULL 约束 |
| inv_M5_04 | 默认归档不物理删除——`archived=1` 级联 chunks/entities，GDPR Art.17 例外 | M2 Forgetting 审计 |
| inv_M5_05 | RRF 融合不裸加权——`weight/(k+rank+1)`，防止不同检索器分数尺度不可比 | HybridRetriever 代码审计 |
| inv_M5_06 | Taint 传播——chunks/events/RetrievedItem 均携带 5 级 TaintLevel，只升不降 | CI `taint_propagation` 测试 |

> 进入此模块前必读 **RAG 链路 + 向量检索工程化阵阱**：`docs/specs/09-LLM-Agent-Production.md`（HE-Rule-1/4 实例: A-07/A-08/A-14 + P-4/P-5）

---

| 记忆层 | 物理存储 | 读写比 | 延迟要求 |
|--------|----------|--------|---------|
| L0 Working | 进程内原生 ContextWindow(Slice) + ScratchPad(sync.Map) [Tier-0] + Immutable Core + NotesStore(SQLite 持久化) | 1:1000 | <1µs |
| L1 Episodic | [Storage-SQLite] session_events + [Storage-SurrealDB-Core] embedding 列 | 100:1 | 写<100µs, 读<5ms |
| L2 Semantic | [Storage-SurrealDB-Core] | 1:50 | <10ms |
| L3 Procedural | [Storage-SurrealDB-Core] skill_id→blob + 语义检索 + 文件系统 SKILL.md | 1:500 | <10µs / <5ms |

---

## 2. L0 Working Memory

### 2.1 核心结构

WorkingMemory/ImmutableCore/ContextWindow/ScratchPad 接口定义见 `internal/protocol/interfaces.go`，实现见 `internal/memory/`。NotesStore 实现见 `internal/memory/`；UserProfile（PersonaRefiner）实现见 `internal/agent/context/`。

**写入权分离**: M11 Policy → ImmutableCore.SafetyConstraints; M9 PersonalizationWorker → ImmutableCore.UserPreferences + InteractionSummary; 用户显式 `/set` → UserPreferences; M5 Memory System → 仅读取，永不写入。

**ContextZone**:
```
ZoneImmutable=0      // 存放系统全局目标、Agent 身份认知(包含底层模型及工具列表)以及用户长期偏好和安全约束，置于最顶部且永不被裁剪
ZoneCoreMemory=1     // 核心工作记忆区，Agent 显式可编辑的持久化区块集合（persona, task_state 等）
ZoneMutableSkill=2   // SKILL.md 描述模板，M9 PromptOptimizer 合法优化靶点
ZoneTaintedData=3    // 外部数据，[TaintLevel] Tracked，永不进入指令区
```

**M9 → ZoneMutableSkill Taint Gate（双层）**:
1. **自动层**: PromptOptimizer 输出 → M11 `SanitizeBySchema` + `SanitizeByDeterministicTransform` → SIC（System Instruction Check，系统指令检查） 检测 → 阳性丢弃 + 审计 `prompt_opt_taint_rejected`
2. **HITL（Human-in-the-loop，人机协同） 层**: 前 5 次写入经 LLM（Large Language Model，大语言模型）-as-Judge 语义审核 → unsafe → [ESCALATE]。累计 5 次通过 → 概率抽查 (20%/次，安全随机源)，优先抽查"语义距离 >2σ"输出（历史 PromptOptimizer embedding 分布）。命中 + unsafe → 撤销该 task_type 全部豁免 + 语义漂移分析（余弦距离 >2σ → CRITICAL）。禁止永久豁免
3. **独立 LLM-as-Judge 二次审查**: 输出合并前必经独立 Judge（不同 Provider 模型）审查。不通过 → 丢弃 + HITL。防 SIC 对间接指令注入的 false negative

**kernel.PromptBuilder 写入门控**:
0. **InteractionSummary 特例**: M9 PersonaRefiner 生成的 InteractionSummary (`source='persona_refinement'` + Ed25519 签名) 写 ZoneImmutable 前执行 `SanitizeByDeterministicTransform`（保留 <200 tokens 摘要 + SHA-256 校验和），TaintLevel 强制 TaintLow。固定白名单——仅 `source='persona_refinement'` + 有效 M9 签名可写 ZoneImmutable
1. zone==ZoneImmutable 且内容 TaintLevel > TaintLow → 编译期阻断：`WriteInstruction`/`WriteSystemPrompt` 参数类型强制为 `substrate.SafeString`，`TaintedString` 无法隐式传入（`prompt.go` 实现），运行时 panic 路径已移除
2. zone==ZoneTaintedData 且内容 Tainted → 接受写入（正确归宿）; zone!=ZoneTaintedData 且 TaintLevel >= TaintMedium → 降级路由到 ZoneTaintedData + WARN + 审计事件 `prompt_builder_taint_zone_routing`
3. zone==ZoneMutableSkill → 验证 Ed25519 ApprovalSignature（M9 签发）→ 签名无效则降级 ZoneTaintedData + WARN + 审计事件 `prompt_builder_mutable_skill_integrity_failed`
4. 签名通过 → Monotonic Version Gate: 查询 `sys_config.min_skill_version`，version < min → 拒绝 + CRITICAL + 审计事件 `prompt_builder_rollback_attack_blocked`
5. 写入对应 zone string builder

**kernel.PromptBuilder.Build**: 固定顺序 ZoneImmutable → ZoneCoreMemory → ZoneMutableSkill → ZoneTaintedData。ZoneTaintedData (以及带有高污点等级的 ZoneCoreMemory 区块) 追加前，对 `[TaintLevel] >= TaintMedium` 的内容执行 M11 Spotlighting 包裹：
  `=== UNTRUSTED_DATA_{sha256(content)[:8]} ===\n{content}\n=== END_UNTRUSTED_DATA ===`
spotlight_hex 由内容 SHA-256 前 8 位派生（非随机）→ 同内容同标记，PromptFn 保纯函数性，M12 Eval 回放可验证。M4 上下文注入同此规则。

**SessionResume（崩溃后 ActiveContext 重建）**:
1. 从 M4 FSM（Finite State Machine，有限状态机） Snapshot 恢复 FSM 状态 + DAG（Directed Acyclic Graph，有向无环图） 进度
2. 从 [Storage-SQLite] `session_events` 按 AUTOINCREMENT 读取 Snapshot 后 Episodic Events
3. 加载 NotesStore 中关联活跃 Note（未过期/未删除）
4. 以 Snapshot WorkingMemorySummary 为基底，重放 Episodic Events → 重建 ActiveContext
5. ActiveContext 就绪 → Agent FSM 恢复执行
约束: <500ms 完成（Snapshot 后事件 <1000 条）。Snapshot 损坏 → 全量重建。Notes 懒加载（仅 5 条热 Note）。>1000 条事件按 200 条/批增量

### 2.2 NotesStore

工作记忆的持久化存储，为 Agent 提供跨 Session 的轻量笔记能力。实现见 `internal/memory/store/notes_store.go`（`SQLNotesStore` / `InMemNotesStore`）。DDL 见 `internal/protocol/schema/023_notes.sql`。

**存储约束**: 单条 Note 上限 64KB（`content`），默认 TTL 为 7 天（`expires_at = now + 7d`）。支持基于系统保留标签（如 `task:{taskID}`）进行任务级别的笔记聚合检索（`ListByTask`）。

**写路径**: 直接 SQL 写（`ON CONFLICT DO UPDATE`，CAS（Compare-And-Swap，比较并交换） `version++`），与 `PromptVersionStore` 同级别。CAS 失败时静默幂等（调用方可重试）。注：原规范要求通过 MutationBus；实际设计中 NotesStore 为 L0 单 Agent 私有状态，不跨 Agent 共享，无需通过 MutationBus 保证跨 Agent 顺序性。

**容量管理**: 过期 Note 由 `GC()` 定期清理（直接 DELETE，调用方负责调度）。

### 2.3 用户画像

实现见 `internal/agent/context/`（PersonaRefiner + UserProfile）。持久化到 `preferences` 表（单条 JSON）。

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

### 2.4 核心记忆分页置换（GD-14-002，最小实现）

`ZoneCoreMemory` 受 `CoreMemoryTotalMaxKB` 硬顶（Tier-0 约束），长程任务容易被已处理完的子任务详情占满每轮可见的稀缺空间。`memory_page_out`/`memory_page_in` 两个内置工具（`internal/tool/builtin/memory_tools.go` + `memory_tools_exec.go`）允许 Agent 自主把不再需要每轮可见的 Core Memory 块置换到 L2 Semantic Memory（entity type `PagedCoreMemory`，name=`paged:{sessionID}:{blockKey}` 复合键防跨会话冲突），置换成功（`SemanticMemWriter.UpsertFact` 先行归档）后才删除原 Core Memory 块，避免中途失败丢数据；`memory_page_in` 做反向操作，未曾页出的 `block_key` 走 `status=not_found` 软失败（与 `memory_expire` 既有约定一致，非错误）。

是否/何时调用完全由 LLM 自主判断——`internal/agent/context/context_pressure.go` 的 `contextPressureHint` 仅将 `sCtx.TokensUsed/TokenBudget` 比例作为信号注入 `BuildPerceiveContext`/`BuildPlanContext` 的 System Prompt（≥65% 温和提示，≥85% 建议调用 `memory_page_out`），不在 Go 侧设强制阈值触发。与 §10 Forgetting 的全局被动回收（效用衰减 + 冷归档）是两套互补机制：前者是 Agent 主动发起的"任务级"置换（可逆、Agent 感知），后者是系统被动的"全局"清理。

---

## 3. L1 Episodic Memory

### 3.1 episodic_events 表

> `episodic_events` 是 M2 `events` 表（[EventLog] 真相源）的派生投影表。
> - **热路径**：`EpisodicMem.Append()` 将事件写入 `kv_store` KV（Key-Value，键值） 表，Payload 上界 8KB，超限落盘至 `~/.polarisagi/polaris/logs/events/<id>.bin` 并替换为 `log_ref` 占位符。
> - **冷投影**：`agent_execute.go` 同步向 outbox 写入 `target_engine="episodic"` 记录，OutboxWorker `EpisodicProjectorHandler` 异步消费并 INSERT `episodic_events` 表，填充 `content`/`salience`/`decay_weight`/`cold` 等字段。

**可变字段白名单**: `episodic_events` 为派生投影表，允许受控字段变更（`archived`, `decay_weight`, `salience`, `archive_offset`），其余字段 append-only。每次受控字段变更须写入 `episodic_events_change_log` 表（再由该 log 表参与 M11 hash chain）。M2 `events` 表（真相源）仅 INSERT，绝不 UPDATE。M11 hash chain 覆盖 `events` 表全字段 + `episodic_events_change_log` 表。

**`[ReasoningState]` 列**（M4 §7.1 跨轮持久化）: `episodic_events.reasoning_state BLOB(nullable)` — Provider 返回的推理状态 blob，msgpack + AES-256-GCM 加密（key 由 [CredentialVault].persistent_key 派生）。同 task_id 30min 窗口内 M4 可读取最近一条注入下次 LLM 调用。SessionPIIVault.SecureZero 同步清零本字段。Tier 0 默认不写（关闭 `FeatureReasoningStateCarry`）。

DDL 和持续性记忆组映射表见 `internal/protocol/schema/003_episodic_memory.sql`。

Salience: LLM 输出 + 工具结果 → 低; 用户反馈 + 关键决策 + 失败/成功 → 高。基于 M9 SurpriseIndex 信号的边权重强化与时间衰减实现动态权重调整（见 §7.6）。

**EpisodicGraphIndexer（GraphRAG 融合）**:
`EpisodicMem.Append()` 写 SQLite 后，同步调用 `EpisodicGraphIndexer.Index(ev)` 在 SurrealDB 图谱中建立边：
- `episodic:{id} → TRIGGERED_BY → agent:{agentID}`（事件来源追溯）
- `episodic:{id} → IN_SESSION  → session:{taskID}`（会话聚类检索）
- `episodic:{id} → ACTION_DONE → entity:tool:unknown`（工具调用关联，`EventActionDone` 类型）

实现见 `internal/memory/`（EpisodicGraphBridge）。边写入为 best-effort（失败仅记日志，不阻断写路径）。通过构造器注入图存储；indexer=nil 时跳过图桥。

**NamespaceID 分区（GD-14-001，多 Agent 协同任务共享记忆命名空间，最小实现）**：`EpisodicMem.Query` 现有的 `ev.TaskID != q.SessionID` 过滤是记忆隔离的唯一真实机制。GD-14-001 复用既有但基本未用的 `types.TaskEntry.Namespace` 字段：CSV Fan-out（`RunCSVFanout`）为同一批子任务统一赋值 `Namespace=jobID`；Worker 认领任务时通过新增的 `AgentKernel.SetMemoryNamespace` 注入 Agent，`Agent.memoryPartitionKey()` 优先返回 NamespaceID（未设置则回退 SessionID）作为写 Episodic 事件时的 `TaskID`，使同一命名空间内的多个子 Agent 共享同一份 Episodic 事件流。仅 4 类"协同性"写入（task_perceived/plan_generated/reflection_completed/execution_completed）参与替换；2PC 幂等 bookkeeping（`agent_execute_dag.go`）与 FastPath 意图缓存显式排除、仍用 SessionID，防止破坏崩溃恢复正确性。命名空间内共享 ≠ 无限制共享——TaintLevel 过滤在共享范围内仍然生效（见 `internal/memory/store/episodic_namespace_test.go` `TestEpisodicMem_NamespaceSharing_TaintStillEnforced`）。Blackboard 侧的字段传播详见 M8 §11.1。

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

**表结构** (`reflection_memory`，DDL 见 `internal/protocol/schema/024_reflection_memory.sql`，实现见 `internal/memory/store/sql_reflection_mem.go`):
- `id` (TEXT PRIMARY KEY), `session_id`, `agent_id`
- `task_type` (TEXT，专用列，`idx_reflect_task_type` 索引覆盖，避免全表扫描)
- `reflection_type` ∈ {success_pattern, failure_mode, efficiency_insight, cross_task_principle}
- `content` (≤500 tokens), `fail_reason`, `strategy`, `decision`
- `salience` (REAL 0-1), `embedding` (BLOB, float16 量化), `embed_model_ver`
- `accessed_count`, `last_accessed_at` (LRU 淘汰依据)
- `evidence_ids_json` (支持该反思的 Episodic 事件 IDs), `meta_json`
- `created_at` (Unix 秒)
- HT0 上限：5000 条，LRU 淘汰批次 100 条

**触发** (后台 worker `ReflectionWorker`，实现见 `internal/learning/reflexion/reflection_worker.go`):
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
- S_PERCEIVE / S_REPLAN 阶段：`buildPerceiveContext` / `buildPlanContext` 额外直接调用 `QueryReflections(Topic=TaskModel.Goal, K=3)` 注入 system prompt（非 ZoneImmutable——反思为 Agent 自生成，TaintLow，但不走 PromptBuilder 写入门控）
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

**[接口约束]** SemanticMemory 的事实/关系写入方法必须在 `internal/protocol/interfaces.go` 中声明，实现见 `internal/memory/`；所有写入必经 MutationBus，禁止绕过 M2 单写者约束直接执行 SQL。实体生命周期管理（标记废弃、列举活跃、UserProfile 读写）属于轻量同步读写，走直接 SQL（不经 MutationBus）。Embedding 存 BLOB（float32→float16 量化，量化工具位于 `internal/llm/`）。

**[XR-16 读写对称]** `taint_level` 写路径已有 only-up 语义（ON CONFLICT 用 `MAX(taint_level, excluded.taint_level)`）。`GetEntity` 读路径同步：SELECT 包含 `COALESCE(taint_level, 0)`，Scan 绑定 `ent.TaintLevel`（ADR-0025（Architecture Decision Record，架构决策记录） BUG-4）。任何绕过此绑定的直读路径视为 XR-16 违规。

### 4.3 QueryClassifier + RetrievalRouter

实现见 `internal/memory/`（QueryClassifier）。

**Tier-0（已实现）**: 纯规则中文关键词匹配（<10µs，无 LLM/embedding 调用）：
- 时间关键词（"最近"/"上周"/"历史"）→ `temporal`
- 操作关键词（"如何"/"怎么做"/"步骤"）→ `how_to`
- 事实关键词（"是什么"/"定义"/"谁是"）→ `factual`
- 推理关键词（"为什么"/"分析"/"比较"）→ `reasoning`
- 规则未命中 → `unknown`（等效全搜）

**Tier-1+（✅ 已实现）**: `ClassifyQuerySemantic(ctx, query, embedder)` 在 `query_classifier_semantic.go` 中；`InitPrototypes` 预计算 4 类原型向量并缓存；运行时余弦相似度比较，置信度 <0.3 回退 `unknown`；embedder=nil 或原型未初始化时自动降级至 Tier-0 关键词路径。

**RetrievalRouter（已实现，在 HybridRetriever.Search 内）**:
- `temporal` → 激活第 5 路 `DurativeMemory`（`DurativeMemoryManager.RetrieveGroups`）
- 其余类型 → 标准 BM25 + Simhash + Graph + ReflectionMem 四路融合
- RRF 权重: BM25=1.0 / Simhash=0.8 / Graph=0.6 / Reflection=0.15 / Durative=0.3

---

## 5. L3 Procedural Memory

程序记忆（技能库）采用三层存储架构: SurrealKV KV 作为热路径的签名级精确查找（skill_id → Wasm 二进制，延迟 <10µs），SurrealDB-Core 作为语义搜索路径（用于 System 2 的 embedding-based 相似技能检索），文件系统 SKILL.md 作为 Ground Truth（技能源码和契约的权威定义，受 Git 版本控制）。

双轨检索: System 1 路径通过 IntentSignature 在 SurrealKV 中做 O(1) 精确匹配（亚毫秒级），命中后由 M6 WasmSkillCache 缓存编译产物直接执行。System 2 路径在 SurrealDB-Core 中做 KNN 语义搜索，返回候选技能后按成功率排序，渐进披露注入 LLM prompt。

L3 Procedural 技能索引相关 DDL 实质托管于 M2 SurrealKV KV 引擎（`internal/store/surreal_store.go`），SKILL.md 元数据从文件系统懒加载。M5 skillKV 与 M6 WasmSkillCache 的关系见 M6 §5.1。

---

## 5-bis. LLM 主动写记忆工具（Agent Self-Memory Writing）

> **概念边界**：§6 "Write Path" 描述的是系统在 Agent 轮次结束后自动触发的被动记忆积累路径（情景→冷投影→语义/图）。本节描述的是 **LLM 在推理过程中主动调用工具、即时写入或更新语义记忆**的能力——两条路径并存，互不干扰。

### 5-bis.1 设计动机

被动积累仅在对话结束后异步落盘，LLM 在当次推理中无法确认某个关键事实已被持久化。主动写工具解决了这个问题：LLM 可在同一轮推理内确认"我已将此事实写入长期记忆"，后续对话可立即检索到。

### 5-bis.2 工具集（`internal/tool/builtin/memory_tools.go`）

| 工具名 | Capability | 语义 |
|--------|------------|------|
| `memory_write` | `CapWriteLocal` | 写入/覆盖一条语义事实（`SemanticMemWriter.UpsertFact`），支持 `valid_until` 过期时长 |
| `memory_search` | `CapReadOnly` | 混合检索（BM25 + vector + graph，`HybridRetriever`），返回最相关事实，支持 `as_of` 时空穿梭查询 |
| `memory_append` | `CapWriteLocal` | 追加属性到已有实体（`UpsertFact` upsert 语义，不覆盖 description） |
| `memory_expire` | `CapWriteLocal` | 标记实体失效（`SemanticMemWriter.MarkEntityExpired`，直接置 `semantic_entities.status='expired'`），含 reason 审计字段 |
| `memory_reflect`| `CapWriteLocal` | 记录系统反思、洞察或长期决策到 ReflectionMemory |

所有 5 个工具 `SandboxTier = SandboxInProcess`、`RiskLevel = RiskLow`，经 PolicyGate 五阶段后在 InProcessSandbox 执行。

### 5-bis.3 注册路径

```
boot_tools.go（或 boot_agent.go）
  └─ builtin.RegisterMemoryTools(sbx, toolReg, semanticWriter, retriever)
        ├─ sbx.Register(tool.Name, fn)         // InProcessSandbox 执行函数
        └─ toolReg.Register(tool)              // InMemoryToolRegistry 工具元数据
```

- `semanticWriter`：`SemanticMemWriter` 接口，实现方为 `internal/memory/store/semantic_mem.go`
- `retriever`：`protocol.HybridRetriever` 接口，实现方为 `internal/memory/retrieval/retriever.go`
- 工具元数据**内联构造**（不走 `tool.LoadBuiltinToolMeta` embed FS），防止 `builtin/<name>/` 目录缺失时静默跳过。

### 5-bis.4 与被动写路径的关系

| 维度 | 被动路径（§6） | 主动写工具（§5-bis）|
|------|--------------|-------------------|
| 触发时机 | Agent 轮次结束后 outbox 异步 | LLM 推理中调用 tool_call |
| 目标存储 | 情景记忆 → 冷投影 → 语义 | 直接写语义记忆（`semantic_memory` 表）|
| LLM 可确认 | 否（异步） | 是（同步返回写入结果） |
| 数据来源 | 系统自动采集对话轨迹 | LLM 显式决策哪些事实值得持久化 |

---

## 6. Write Path: Hot/Cold 分离

Agent 动作完成后，写入路径拆分三条线:

**热路径 1（同步，kv_store）**: `EpisodicMem.Append()` 将事件序列化写入 `kv_store` KV 表，Payload 超 8KB 门控截断 + 落盘。同步更新 Working Memory 缓存，触发 TokenBurnRate 计数。不调用 LLM，不操作图，不等 embedding API。

**热路径 2（outbox 写入）**: `agent_execute.go` 在写 kv_store 后同步向 outbox 投递 `target_engine="episodic"` 记录，触发冷投影。

**冷路径（OutboxWorker 异步）**: `EpisodicProjectorHandler` 消费 outbox 记录 → INSERT `episodic_events`；`OnlineReindexer` 扫描 `embed_model_version=''` AND `cold=0` 的行 → 调用 M1 Embedding API 填充向量；其余 handler 执行图写入、实体/关系 Upsert、Consolidation 阈值检查、Skill 成功率统计。`cold=1` 的历史事件跳过 embedding 生成。

---

## 7. Read Path: HybridRetriever

实现见 `internal/memory/`（HybridRetrieverImpl）。

HybridRetrieverImpl 持有 store / graph / durative / reflectionMem / embedder 五个可选路径字段，提供多个构造器重载，按激活路径数量递增（从基础 BM25+Simhash 到含 Graph+DurativeMemory+ReflectionMem 的全路径）。向量检索路径可在构造后注入。

6路融合检索（BM25+Simhash+Vector+Graph+ReflectionMem+DurativeMemory），RRF（k=60）融合排序，权重分别为 1.0/0.8/0.6/0.6/0.15/0.3。

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
2. 扩散: energy × edge.weight 传播至邻居，自身 ×0.7 衰减（`energyDecay=0.7`）；energy ≤0.05 停止（dormancyThreshold）
3. 最多 5 轮（fanOutLimit=5）
4. 按最终 energy 降序 topK
5. 模式选择: query 含"为什么"/"原因"/"关联"/"影响" → Spreading Activation; 否则 BFS

### 7.4 RRF 融合 + BM25 精排

当前实现为 6 路 RRF 融合（`internal/memory/`，HybridRetrieverImpl）。

**6 路召回路径**（按 scope=memory 路由）：
- BM25（weight=1.0）：KV 前缀扫描 + TF×IDF 近似，Tier 0 纯 Go，无 FTS5 依赖
- Simhash（weight=0.8）：64-bit 指纹，汉明距离 ≤16，sim=1-(dist/64)
- Vector（weight=0.6，embedder 非 nil 时激活）：KNN 余弦距离，embedder==nil 时跳过
- Graph（weight=0.6，Tier1+，graph==nil 跳过）：BM25 Top1 作起点图遍历，跳数衰减赋分
- ReflectionMemory（weight=0.15）：SQL QueryReflections（idx_reflect_task_type 索引），nil 时降级 KV 扫描
- DurativeMemory（weight=0.3，ClassifyQuery==Temporal 激活）：RetrieveGroups(5) 摘要参与 BM25 打分

**RRF 融合**：`score(d) = Σ weight_i / (k + rank_i + 1)`，k=60（见 `spec/state.yaml §m5_memory.rrf_k`）。截断 FinalTopK=20（config.FinalTopK 可覆盖）。

**污点传播**：RRF 聚合阶段同步维护 taintMap（只升不降规则），ScoredFragment.TaintLevel 按来源前缀赋值：episodic/durative_group 前缀 → TaintHigh，其余 → TaintMedium（inv_M5_06 实现，见 `internal/memory/`）。调用方在将 ScoredFragment 注入 Prompt 前须遵循 PropagateTaint 规则。

隐私门控由上层 M11 Policy Gate 在 ctx 中注入，retriever 不内联 ACL。

**M5/M10 共享关系**：检索范围不同（M5: episodic+semantic 前缀，M10: doc_nodes/chunk: 前缀），`HybridRetrieverImpl` 通过 `scope.Type` 参数路由，无需两套实例。M10 参数差异见 M10 §2.2。

**跨版本嵌入兼容**：`MemoryEntry.EmbedModelVersion` 字段触发 `OnlineReindexer`（inv_M5_03）；当前 Tier 0 BM25+Simhash 路径不依赖 embedding API，嵌入模型变更不影响 Tier 0 召回。

**OnlineReindexer（批量重建 embedding 索引）**：

实现见 `internal/memory/`（OnlineReindexer）。

- **触发条件**：`episodic_events.embed_model_version = ''`（OutboxWorker 尚未投影）或 `!= currentVersion`（模型切换）；走 `idx_ep_embed_ver` 偏索引，O(1) 量级扫描
- **接口**：`Embedder`（consumer-side，`Embed(ctx, text) ([]float32, error)` + `ModelVersion() string`）——M1 EmbeddingBatcher 实现此接口注入，防包循环
- **批处理**：`Run(ctx)` 每次取 50 条（`defaultReindexBatchSize`），每条 embed 后 `runtime.Gosched()` 让出调度，单条失败不中断整批（best-effort）
- **量化**：float32 → float16 BLOB（IEEE 754 half-precision，小端序），与 DDL 003/004 规范一致；精度损失在 RRF 归一化路径中可接受
- **返回**：`(processed int, remaining bool, err error)`；调用方循环调用直至 `remaining=false`；version 切换场景由调用方决策重触发，避免无限循环

### 7.5 Temporal Retrieval (AsOf)

HybridRetriever 支持基于 `as_of` 时间戳的时空穿梭查询。当提供 `as_of` 参数（非 0）时：
- 语义记忆的实体在命中解析时，将检查其生命周期有效区间（`valid_from` 和 `valid_until`）。
- 若事实在 `as_of` 时间点未生效或已过期，则在结果中过滤，从而实现在不影响数据库当前活动状态的前提下查询历史事实版本。

### 7.6 Evidence Subgraph Extraction

**EvidenceSubgraphExtractor**（`internal/memory/graph/edge_weight.go`）:
负责知识图谱的子图提取。基于种子实体执行有界 BFS（maxDepth=2，maxNodes=50）+ alpha 权重过滤（Personalized PageRank 近似剪枝），输出结构化实体列表辅助证据链检索。邻接表 key 格式：`adj:<entityID>`；边权重 key：`edge_w:<src>_<dst>`（见 upgrade-10）。

### 7.6 Edge Weight Reinforcement & Decay

**EdgeWeightManager**（`internal/memory/graph/edge_weight.go`）:
控制图谱边权重的生命周期。`DecayUnused`: 读时 O(1) 指数衰减计算（防写放大）。`ReinforcePath`: 强化经过路径。`FeedbackCalibrate`: 成功轨迹校准，每条边 +0.03，上限 1.0，持久化至 KV `edge_w:<edgeID>`。`PeriodicPrune`: 扫描 `edge_w:` 前缀，批量删除 weight < pruneThreshold 的边。

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

**5-Stage Pipeline** (`internal/memory/`):

- **Stage 1 — 实体/关系提取**: 
  - 聚合 Session 内 Episodic 事件文本，优先调用 Provider LLM 提取命名的限定实体（`user_preference`, `constraint`, `temporary_conclusion`, `entity`）与特定关系（`depends_on`, `configures`, `conflicts_with`, `relates_to`, `derived_from`），结构化输出 JSON。`derived_from`（GD-14-001 新增）专用于标注"一个实体是另一实体的推论/派生结论"，区别于运行时/配置依赖的 `depends_on`，供下方级联失效识别真实派生血缘。
  - LLM 不可用时降级为正则模式匹配。
- **Stage 2 — Upsert Semantic + Entity 生命周期 + 级联失效**: 
  - 批量调用 `SemanticMemory.UpsertFact / UpsertRelation`，经 `retrieval.ExclusiveWriter` 闭合旧事实。
  - 精确名称冲突 → `MarkEntitySuperseded(oldDBID)` 打标 `superseded` 后 INSERT 新版本。
  - `user_preference` 类型还额外执行 Jaccard 近重复检测（阈值 0.6），Jaccard 命中的旧实体同样打标 `superseded`，并与精确名称冲突分支共用同一级联失效触发路径（2026-07-12 复核修复：此前 Jaccard 分支只打标 `superseded`、未触发下方 `CascadeInvalidator`，与精确冲突分支行为不一致）。单条失败不中止批次。
  - **`[CascadeInvalidator]`**（`internal/memory/retrieval/cascade_invalidator.go`）：`ExclusiveWriter` 成功闭合旧事实后触发，沿 `semantic_relations` 图（覆盖全部 relation_type，含 `derived_from`）用一条 SQLite 递归 CTE（`WITH RECURSIVE`）从被取代实体出发扩散，最多 2 跳（`maxCascadeHops`，防全图扩散），命中的相邻实体批量置为 `status='pending_review'` 并写入 `episodic_events_change_log` 审计记录，交由后续人工/自动复核，而非直接删除或自动改写——解决"基础信念变更时其派生推论未被连带标记"的问题。递归 CTE 用 path 列防止双向边折返父节点，hop 列保证有环图严格有界终止。
- **Stage 3 — 会话摘要**: 
  - LLM 生成 3-5 句摘要（source='compaction'），写入 `SemanticMemory.StoreDocument`；LLM 不可用时降级为事件类型频次拼接摘要。
- **Stage 3.5 — UserProfile 合成（L3 Persona）**: 
  - `len(events) >= 10` 时触发。优先 LLM 路径（512 token 预算）从 Episodic 事件批量提取 `stable_facts`/`recent_activity`/`behavioral_patterns`。
  - LLM 不可用时降级为规则路径（工具调用频次 + 事件类型统计）。结果写入 `user_profile` 表。非阻塞，失败静默跳过。
- **Stage 4 — Logic Collapse → Skill Library**: 
  - 统计 Session 内同名工具成功调用次数，≥ 3 次时将该工具注册为 `SkillMeta`（`SkillRegistry.Register`）；SkillRegistry 为 nil 时跳过。

> **约束**：version++ 不可变版本 + source_event_id provenance + 信念修正 + 单条失败不中止整体管线。

**依赖注入**: `NewConsolidationPipeline(episodic, semantic, skills, summarizer)` — episodic/semantic 必须非 nil；skills 和 summarizer 可选（nil 时对应阶段降级或跳过）。完整版 `NewConsolidationPipelineFull` 额外注入 `writeFilter`（写入前过滤）、`cascadeInv *retrieval.CascadeInvalidator`（Stage 2 级联失效）、`db`。summarizer 是 `memory.LLMSummarizer` 接口（`Summarize` + `InferRaw` 两个方法），2026-07 复核修复替换了此前直接持有 `protocol.Provider` + 硬编码 prompt 字符串拼接的实现，Stage 1 实体抽取与 Stage 3.5 UserProfile 合成均改为经 `internal/prompt/templates` 渲染模板 + `InferRaw` 调用。

**超时保护（已修复）**: `Run` 在调用方未设置 deadline 时自动注入 5 分钟超时，防止 LLM 长调用阻塞 M9 调度器。实现见 `internal/memory/`。

**冷启动 IO 锁（已修复）**: `EpisodicMem.Query` 冷启动路径已将 `store.Scan` 移至锁外执行，锁内仅合并结果到 `em.events`，消除 IO 阻塞风险。

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

## 11. Context Assembler / PromptBuilder

PromptBuilder 布局实现见 `internal/agent/`（PromptBuilder），SessionCompressor 实现见 `internal/memory/`（SessionCompressor）。

### 11.0 系统提示词三层组装

系统提示词在 `ImmutableCore.PrependToMessages()` 中按三层顺序组装，对应 KV Cache 由稳定到易变的排列原则：

| 层 | 内容 | 变更频率 | 文件 |
|----|------|---------|------|
| stable | Agent 身份（SOUL.md 或 `DefaultPolarisIdentity`）+ 模型感知工具调用引导 + 平台感知提示 | 跨会话不变 | `internal/memory/` |
| context | 项目上下文（由 M13 Interface 层注入 ambient skills / 全局目标等） | 会话内不变 | `internal/gateway/` |
| volatile | 当前日期（精确到天，不到分钟，避免逐分钟破坏 prefix cache） | 每天变一次 | `ImmutableCore.VolatileBlock` |

**用户自定义身份**：`~/.polarisagi/polaris/config/SOUL.md` 存在时覆盖 `DefaultPolarisIdentity`，服务启动时一次性读取并缓存到 `Server.soulMDContent`。

**模型感知引导**：`memory.NeedsToolUseEnforcement(modelID)` 判断模型族（deepseek/qwen/gpt/gemini 等），对应注入 `memory.ModelSpecificGuidance(modelID)` 的专属工具调用约束文本，防止模型仅描述意图而不实际调用工具。

**平台感知提示**：按接入平台（cli/webui/api/cron）注入不同的输出格式和行为说明（实现见 `internal/memory/`）。`POLARIS_PLATFORM` 环境变量控制，默认 `webui`。

**M9 激活路径**：`PromptVersionStore.OnActivate` 回调在 `task_type='general'` 的版本激活后触发，热更新 `Server.activatedSystemPrompt`，下一轮 `injectSystemPrompt()` 生效（goroutine-safe，受 `activatedSystemPromptMu` 保护）。`SystemPromptTemplate` 非空时全量走模板渲染，跳过三层组装，允许 M9 完全接管系统提示词内容。

### 11.1 三所有权层

提示词所有权明确分为三层，变更边界清晰：

| 层 | 存储位置 | Owner | 变更方式 | DB（Database，数据库） 重置（re-init）行为 | Factory Reset 行为 |
|----|---------|-------|---------|---------------------|------------------|
| **Layer 0** 内置默认 | `configs/prompts/*.md`（go:embed，随二进制） | Polaris 项目 | PR + 重新编译 | 不受影响（embedded） | 不受影响 |
| **Layer 1** 用户自定义 | `~/.polarisagi/polaris/config/prompts/*.md` | 用户 | 直接编辑文件 或 `PUT /v1/config/prompts/{name}` | **存活**（文件不在 DB 中） | `DELETE /v1/config/prompts/{name}` 显式删除 |
| **Layer 2** M9 优化 | DB `prompt_versions` 表 | 自进化引擎 | M9 自动生成 + Staging 审批 | **重置**（随 DB 删除，正确） | 随 DB 删除 |

**用户可通过 API 编辑的提示词**（Layer 1，白名单控制）：
- `identity`：Agent 身份文本（覆盖 Layer 0 identity.md，整段替换）
- `custom_instructions`：追加的行为指令（拼接到身份之后，不覆盖）

不暴露给用户的提示词：tool_enforcement（产品行为逻辑）、platform hints（格式化指令）。

**API**: `GET/PUT/DELETE /v1/config/prompts/{name}`（实现见 `internal/gateway/`）。

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

**SessionCompressor** 实现见 `internal/memory/`。

> 与 M4 ContextWindowManager 协同：M4 §7 持有热路径阈值（>70% salience 排序候选 / >90% 语义结构感知逐出），M5 SessionCompressor 在 M4 调用时执行实际的冷压缩算法（本节定义）。

压缩 Stage（由 M4 ContextWindowManager 调用，不独立设阈值）:
- **Stage 1**: tool output pre-pruning——超过 10KB 的 `tool_result` 替换为存根 `[offloaded: N bytes → read_tool_ref("xxx")]`，原始内容经 `ToolRefOffloader` 落盘；立即释放 token，可按 node_id 按需回取
- **Stage 2**: LLM 锚点摘要——以 currentSummary 为锚点追加新事件产生增量摘要（由上层 M4 调用后写入 `SetAnchor`）
- **Stage 3**: **TaskMermaidCanvas 注入**——将 `TaskMermaidCanvas.Render()` 输出（`graph LR` 有向图）前置注入 anchor，形成 `## Task State (node_id → read_tool_ref)\n{mermaid}\n## Summary\n{anchor}` 结构；画布为空时跳过注入。来源：TencentDB Agent Memory 符号化短期记忆（61% token 节省原理）

**TaskMermaidCanvas**（`internal/memory/`）：线程安全符号画布，追踪工具调用的 pending/success/fail 状态并自动连边，输出 Mermaid `graph LR`。节点上限 30，估算约 8 token/节点（20 节点 ~160 token）。

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

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| HybridRetriever 单路检索失败 | 其余路权重接管（DenseVec 失败 → BM25×0.7 + Graph×0.3） | 故障路恢复后自动切回默认权重 |
| Embedding API 不可用 | Simhash 64-bit 指纹备选（汉明距离 ≤8）+ BM25 + 图遍历三路融合 | API 恢复后 DenseVec 权重切回 |
| SurrealDB-Core 维度切换 | 动态路由查询至对应维度表（如 index_local_384），无需强制降级 BM25 | 后台静默回填增量 |
| Consolidation LLM 调用超时 | 跳过本轮 Consolidation，事件保留在 episodic_events 等待下一轮 | 下个 cron 周期自动重试 |
| NotesStore CAS 乐观锁冲突超限（>3 次） | 写入 notes_conflict_log shadow 表供人工裁决 | — |
| Episodic 冷路径 Outbox 积压 | 暂停非关键 Consolidation + WARN | 积压降至 <200 恢复正常 |
| Mem-L3 SurrealKV 引擎故障 | 降级 SQLite 备份索引 (skill_id→metadata，不含 blob) | SurrealKV 恢复后切回 |
| DurativeMemory 聚类 LLM 超时 | 按纯向量余弦相似度聚类（跳过 LLM 语义判定） | LLM 恢复后追加语义连续性标注 |
| Context 组装超时 (>500ms) | 跳过 ZoneMutableSkill 组装，仅发 ZoneImmutable + 摘要 | 下次 context refresh 完整组装 |
| EdgeWeightManager 图边修剪 LLM 超时 | 仅物理 DELETE < pruneThreshold 的边，跳过 FeedbackCalibrate | 下个 cron 周期重新校准 |
| Embedding DriftDetector 检测到漂移 | 该 task_type 降级纯 BM25，其余不受影响 | Blue-Green 重嵌完成后切回 |

与 OSMemoryGuard 协同: L1 预警 → 暂停 Consolidation 冷路径 / L2 紧急 → 暂停 Episodic 冷路径 Outbox 处理、限制 WorkingMemory 容量 / L3 临界 → 全部冷路径暂停，仅热路径写入 + L0 WorkingMemory 读取可用。

---

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m5_memory`。

## 15. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M1 Inference | Embedding API（向量生成）、LLM 调用（摘要/Consolidation） | M1 §6.1, §5 |
| M2 Storage | Store 接口、EventLog 真相源（events 表 → episodic_events 派生投影） | M2 §1.1, §2.1 |
| M4 Agent Kernel | PromptBuilder（三 Zone 组装 + Spotlighting 门控）、HybridRetriever 上下文检索 | M4 §2, §10 |
| M6 Skill Library | L3 Procedural Memory 技能索引 + SurrealKV 缓存 | M6 §7 |
| M9 Self-Improve | M9→ZoneMutableSkill Taint Gate（双层）、Preference Learner、PersonaRefiner | M9 §1.1, M5 §2.1 |
| M10 Knowledge RAG（Retrieval-Augmented Generation，检索增强生成） | HybridRetriever 共享引擎（`internal/store/`）、检索配置差异 | M10 §2.2 |
| M11 Policy Safety | SafetyRules 注入 ImmutableCore、TaintGate 写入 Zone 校验 | M11 §2, M5 §2.1 |
| 全局字典 | HE-Rule-4 数据驱动迭代、HybridRetriever/RRF 定义 | 00-Global-Dictionary §2, §9-bis |
| DDL | 001_events（真相源）、003_episodic_memory（派生投影）、004_semantic_memory（语义层） | internal/protocol/schema/001-004_*.sql |

---

## 16. 实现状态与 2026 研究对照

### 实现记录

| 组件 | 文件 | 状态 |
|------|------|------|
| L2 SemanticMem | `internal/memory/` | **✅ 已完成** — 接口对接 MutationBus 写路径，支持直读查询，淘汰旧版 JSON KV 占位 |
| Entity 生命周期 | `internal/memory/` `internal/protocol/schema/004_semantic_memory.sql` | **✅ 已完成** — 新增 status/superseded_by 字段；Consolidation Stage 2 引入 Jaccard 近重复检测（阈值 0.6，user_preference 类型）；检索路径过滤 active 状态 |
| UserProfile 合成 | `internal/memory/` `internal/protocol/schema/004_semantic_memory.sql` | **✅ 已完成** — `user_profile` 新表；Consolidation Stage 3.5，`events≥10` 触发，LLM 路径（512 token）+ 规则降级；UserProfile 查询/写入接口实现 |
| TaskMermaidCanvas | `internal/memory/` | **✅ 已完成** — 线程安全 Mermaid `graph LR` 画布；SessionCompressor 新增 Stage 3 注入逻辑；12 个单元测试覆盖节点样式、截断、Jaccard、Compressor 集成 |
| EpisodicGraphBridge | `internal/memory/` | **✅ 已完成** — ACTION_DONE 事件解析提取 tool_name 构建动态图边，移除硬编码 |
| ReflectionMem | `internal/memory/` | **✅ 已完成** — 实现了基于 HT0 限额的 LRU 缓存驱逐与存取机制 |
| DurativeMemoryManager | `internal/memory/` | **✅ 已完成** — 实现了持续性记忆组 DurativeGroup 的聚类合并引擎 |
| EdgeWeightManager | `internal/memory/` | **✅ 已完成** — 实现了动态读时图边衰减以及 BFS 子图抽取引擎 |
| ReflectionWorker | `internal/learning/` | **✅ 已完成** — 实现后台提取，在 session 完成后执行经验萃取入库 |
| CascadeInvalidator | `internal/memory/retrieval/cascade_invalidator.go` | **✅ 已完成**（GD-14-001）— 由 `ExclusiveWriter` 在 Consolidation Stage 2 闭合旧事实后触发，沿 `semantic_relations`（含新增 `derived_from` 关系类型）用单条 SQLite 递归 CTE 扩散 2 跳，命中实体标 `pending_review` 并写审计日志 |

### 引入计划（优先级排序）

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **D-MEM 多巴胺门控巩固** | arXiv:2603.14597, 2026 | 以 SurpriseIndex 信号作门控，仅 surprise > 阈值的情节事件才晋升语义层，消除冗余写入与 O(N²) 延迟 | `internal/memory/` + `internal/learning/` | P1 |
| **Path-Constrained Retrieval** | arXiv:2511.18313, 2025 | BFS 遍历限定关系类型白名单（uses/depends_on/extends），防止多跳推理语义漂移 | `internal/store/` (HybridRetriever GraphTraverse) | P2 |
| **E-mem 多 Agent 情节重建** | arXiv:2601.21714, 2026 | 异构辅助 Agent 维护未压缩情节上下文，token -70%，F1 +54%；当前单节点情节记忆在 swarm 场景是盲点 | `internal/swarm/orchestrator/`（中期） | P3 |


## 13. MemoryAgent (Swarm Integration)

The `MemoryAgent` operates as a specialized background worker within the swarm architecture (`internal/swarm/agents/memory_agent.go`). It acts as the "Memory Caretaker" and has three primary responsibilities:

1. **Contextual Whispers (Whisper Channel):** It periodically scans `episodic_events` for recent, unarchived events with high salience (`>=0.7`) that are relevant to the current active goal/blackboard task. It pushes these relevant historical experiences to the active Agent via the `whisperChan` (`protocol.MemoryWhisper`).
2. **Graph Maintenance (Periodic Prune):** It calls the `SynapticPlasticityManager` and `EdgeWeightManager` to apply decay to unused graph edges and prune relationships that have fallen below the threshold.
3. **Memory Pressure Relief:** If the system is under extreme memory pressure, it throttles operations and assists in garbage collection strategies.

**Crucially, the MemoryAgent NO LONGER performs duplicate L1->L2 distillation (semantic compression).** All distillation is handled by the `ConsolidationPipeline` triggered by Outbox events.
