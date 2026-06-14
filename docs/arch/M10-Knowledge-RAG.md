# 模块 10: Knowledge & RAG

> 消费 `[Storage-SQLite]` + `[Storage-SurrealDB-Core]`，非独立存储 | Hybrid Search + GraphRAG | 增量索引 | 来源追踪
> Go 检索流水线 + GraphRAG，Rust SurrealDB-Core FFI 侧车
> `[Code-Package-Mapping]`: pkg/swarm/ | `[Module-Topology]`: M10 L2 | `[HE-Rule-5]` `[HE-Rule-6]`
> **§跳读**: 0-bis:7 职责 / 0-ter:20 不变量速查 / 1:33 摄入 / 2:104 检索 / 3:186 增量索引 / 4:208 来源追踪 / 5:243 Reranking / 6:259 检索质量 / 7:265 数据流闭环 / 9:273 (SOFT)降级 / 10:291 跨模块契约
## 0-bis. 职责边界

| M10 **是** | M10 **不是** |
|-----------|-------------|
| 外部文档摄入流水线（Plugin → Ingester → 层级文档树） | 记忆的读写管理（那是 M5） |
| 层级知识检索（结构化导航 + Hybrid 内容检索 + 上下文展开） | HybridRetriever 底层引擎实现（`pkg/substrate/hybrid_retrieve.go` 为 M5/M10 共享） |
| GraphRAG 知识图谱构建与双模式检索（Local/Global Search） | 图存储引擎（那是 M2 SQLite/邻接表或 SurrealDB-Core） |
| 多级预计算摘要生成（段落→章节→文档） | LLM 调用路由（那是 M1 Provider Router） |
| 增量索引 + 来源追踪（ChunkProvenance） | 嵌入模型选择（Embedding 调用走 M1 Provider Router） |
| 知识源同步调度（基于 Plugin 生态获取数据） | 第三方平台 API 鉴权与网络请求（下放至独立 Plugin 沙箱隔离） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M10_01 | M10 不是独立存储——消费 M2 Store 接口，共享 M5 HybridRetriever 底层引擎 | 架构审计 |
| inv_M10_02 | 知识源拉取内容默认 taint=high——对于受信任的本地文件系统插件，可按策略降为 low | M11 Connector-Taint-Table |
| inv_M10_03 | 每 chunk 携带 lineage metadata——source_uri + doc_version + chunk_seq + content_hash | DDL NOT NULL 约束 |
| inv_M10_04 | Embedding 维度变更时禁止跨模型投影——通过 SurrealDB-Core 双索引隔离表实现无缝切换 | M10 §1.5 |
| inv_M10_05 | GraphRAG LLM 取消严苛日预算限制（得益于 DeepSeek 的低成本）——全量生成高质量图谱 | M10 §2.7 预算释放 |
| inv_M10_06 | 主引擎 M10 彻底剥离出站网络权限——任何第三方 API 调用必须在沙箱化的 Plugin 进程中执行 | 代码审计（M10 内无 HTTP Client） |

---

## 1. 文档摄入流水线

### 1.1 层级索引管道 (6 阶段)

1. Knowledge Plugin (MCP) 拉取 → 统一 `Document` 格式
2. 结构解析 → Document Tree（标题层级 → 节点树）
3. 层级分块 + 父子双存
4. 多级摘要生成（后台 LLM，不阻塞摄入）
5. 元数据富化 + Embedding
6. 多引擎索引: `[Storage-SQLite]` doc_nodes + `[Storage-SurrealDB-Core]` 向量 + FTS5 全文

### 1.2 知识源摄入 (Plugin-driven + 直连 Connector)

**直连 Connector（已实现，Tier 0 内置）**:
- `ObsidianConnector` (`pkg/swarm/knowledge/obsidian_connector.go`): 本地 Markdown 目录监听，基于 fsnotify 实时推送变更事件（created/updated/deleted），List/Fetch/Watch 三接口完整，支持 YAML frontmatter 解析。
- `SyncScheduler` (`pkg/swarm/knowledge/sync_scheduler.go`): 消费 `KnowledgeConnector.Watch` 事件并驱动 `KnowledgePipeline.Ingest/Delete`。启动时执行全量初始同步，后切入增量 Watch 模式。防抖窗口 500ms 合并同一文件的连续变更，指数退避重试最多 3 次。

**Plugin-driven Ingestion（长期架构目标）**:
M10 长期方向是消费 `M13-bis Extension Registry` 中声明了 `capability: knowledge_provider` 的 Plugin（MCP 协议），将外部知识获取完全下放至插件沙箱（L2/L3），零信任边界。短期阶段 ObsidianConnector+SyncScheduler 作为 P0 内置 Connector 使用。

**DocTree 持久化**: `PipelineImpl.Ingest` 将文档分块写入 `rag_chunks` SQLite 表（FTS5 支持）的同时，将 DocTree 序列化为 JSON 写入 `rag_docs` 表（uri 为主键，ON CONFLICT DO UPDATE 幂等）。

**可观测性**: 同步延迟 >1800s → ALERT + 暂停非关键知识源。

### 1.3 文档树数据结构

DocNode/LeafChunk/ParentChunk/Chunk 类型定义见 `pkg/swarm/knowledge/rag.go`。DocTree 在 `rag_docs` 表持久化（tree_json 列）。

### 1.4 结构解析 + 父子双存

`DocumentParser` 按 SourceType 选择解析策略（设计目标：Markdown→goldmark AST、PDF→pdfcpu 布局、代码→tree-sitter AST 函数/类、Web→goquery+readability、纯文本→空行+缩进启发式），生成以 document 为根节点的 DocNode 树。

`Ingester.ParseAndBuildTree` 在解析后分层生成 LeafChunk（~256 tokens 语义断点）和 ParentChunk（SectionPath + 前同级 TopicSentence + 完整内容），后台 goroutine 生成多级摘要，`extractStructuredContent` 提取表格 schema 和代码块元数据。

> **当前实现**：`rag_impl.go` 已实现父子双存，`chunkDocument` 采用语义边界切分（✅ 已修复）：先按 `\n\n` 切段落合并为 ParentChunk（≤1000 runes），再按中英文句子结束符（。！？；/ `.` `!` `?` + 空格）切 LeafChunk（≤250 runes），无边界时兜底字符切；结构化解析器（goldmark/pdfcpu/tree-sitter）和多级摘要生成**[计划中]**尚未接入。

### 1.5 嵌入维度变更 (三相渐进恢复)

M1 Embedder 模型切换致维度变更时，禁止全量同步重嵌 (`[Tier-0-Limit]` 8GB下不可行)。

| 阶段 | 触发 | 操作 | 延迟 |
|------|------|------|------|
| Phase 1: 双索引无缝切换 | Embedder.Dimension() 变更 | SurrealDB-Core 内维护单张动态维度表（`vec_dim` 列随模型切换更新）；切换模型时将检索请求路由至新维度索引。**禁止永久降级 BM25**。 | <1ms |
| Phase 2: 优先级热恢复 | Phase 1 后 | 优先级队列重嵌 Top 50 Chunk: 0.4×M5 WorkingMemory引用频次 + 0.3×DurativeGroup活跃度 + 0.2×7天查询频率 + 0.1×访问时间倒数。每Chunk 2s超时，原子替换 | ~25-75s |
| Phase 3: 全量回填 | CPU idle>70% + 空闲内存>500MB | 后台单线程, 5文档/批+1s冷却 | ~5.5h (10K docs) |
| BM25 永久降级 | >30天未访问 | 不重嵌，永久BM25 | 0 |

跨模块: M5 OnlineReindexer 检测维度变更（实现位于 `pkg/cognition/memory/online_reindexer.go`，原地 UPDATE `episodic_events.embedding`，无 shadow table）, M1 Embedder.Init() 暴露 Dimension(), M10 通过 `internal/config/runtime.go` embeddingDim atomic 获取, M5 ActiveDocumentTracker 提供文档热度

### 1.6 多级摘要生成

文档(~200 tokens) → 章节(~100 tokens) → 段落(~20 tokens)。三级树状预计算。

三级预计算（段落≤30 tokens 优先取首句零LLM、章节~100 tokens 子主题拼接LLM、文档~200 tokens 章节摘要+标题LLM），速率限制 MaxDocsPerHour=100、MaxTokensPerDoc=700，超限跳过。摘要 Taint 只升不降，存储于 `summary_taint_level` 列供 M4 TaintContextPropagation 门控。

> ✅ **已修复**：`PipelineImpl.Ingest` 无任何摘要生成逻辑，摘要索引不存在（P1 缺陷）。

摘要 Taint: LLM摘要最低 `[Taint-Floor-Medium]`。段落级→继承源段落TaintLevel, 章节级→子节点max, 文档级→章节max。`[Taint-Prop]` 只升不降。存储列 summary_taint_level(INT 0-4), 供 M4 TaintContextPropagation 门控

### 1.7 内容感知分块

| 类型 | 叶节点 | 父节点 | 特殊处理 |
|------|--------|--------|---------|
| Markdown | 按段落,~256 tokens,语义断点 | 完整段落+章节路径+前文 | 表格保留schema,代码块保留语言 |
| 代码 | tree-sitter AST 函数/类 | 完整函数体+文件/包路径 | import block 独立索引 |
| PDF | 布局感知段落+表格单独提 | 完整段落+章节标题+页码 | 图片提取alt text |
| Web | main content 段落,~256 tokens | 完整section/article | 去除导航/广告/页脚 |
| 对话 | 按turn切分 | 前后2turn完整上下文 | 标注speaker |

---

## 2. 层级知识检索

### 2.1 三阶段结构化检索

1. **结构化导航**: query→embed→摘要层搜索(文档级+章节级摘要向量)→锁定目标 DocNode
2. **内容检索**: 目标子树内 Hybrid Search(BM25+Dense+实体图)→Top50→命中 LeafChunk
3. **上下文展开**: LeafChunk→ParentChunk(完整段落+章节路径+前文衔接+来源追踪)→prompt context

### 2.2 HybridRetriever (内容层)

HybridRetrieverConfig: BM25Weight=0.3, VectorWeight=0.6, GraphWeight=0.1, RRF_K 见 `spec/state.yaml §m5_memory.rrf_k`（M5/M10 共享），OversampleN=3, RerankTopM=50, FinalTopK=5

4 级流水线：三路并行宽召回（BM25 + Dense Vector + Graph Traverse，限定 scope 子树，部分路失败降级继续）→ RRF 融合 → SurrealDB-Core BM25 Reranker（Top RerankTopM=50）→ FinalTopK=5 截断。

共享 `pkg/substrate/hybrid_retrieve.go` 底层 RRF+Rerank 引擎。引擎提供统一接口: `Search(ctx, query, scope, config) → []ScoredFragment`。接口内联的三个检索器 (BM25/DenseVec/GraphTraverser) 通过依赖注入绑定各模块的实际存储后端。M5检索 episodic_events+semantic_entities(跨层并行, scope=memory), M10检索 doc_nodes(先导航再检索, scope=document_tree)。差异锁定在 `RetrievalConfig`:
| 参数 | M5 | M10 |
|------|-----|------|
| BM25Weight | 0.3 | 0.3 |
| VectorWeight | 0.6 | 0.6 |
| GraphWeight | 0.1 | 0.1 |
| OversampleN | 3 | 3 |
| FinalTopK | 10 | 5 |
| RerankTopM | 30 | 50 |
| RRF_K | 60 | 60 |

### 2.3 StructuredNavigator (目录层)

查询在 `rag_chunks_fts`（FTS5）中搜索 `chunk_type='summary'` 的摘要块，取 BM25 rank 最高的 doc_id 定位目标文档，RelevanceScore<阈值时 fallback 全局全文搜索。**注**：`rag_chunks` 表无 `embedding` 字段，向量索引在 SurrealDB-Core；StructuredNavigator 使用 BM25 FTS5 而非向量搜索。实现见 `pkg/swarm/knowledge/rag_impl.go` `StructuredNavigator`。**FeatureGate 门控**：`FeatureDeepRAG`（Tier 1+，≥16GB）；Tier 0 自动退化为全文搜索。

### 2.4 QueryPlanner

简单查询（<30 tokens）直接跳过分解，走单路检索。复杂查询 LLM 分解为 2-5 个子查询，每个子查询携带 TargetScope 和 Weight，支持 concat/deduplicate/interleave 三种合并策略。**FeatureGate 门控**：`FeatureDeepRAG`（Tier 1+）；Tier 0 跳过分解步骤。

### 2.5 KnowledgeBase.Search (完整入口)

实现见 `pkg/swarm/knowledge/rag_impl.go` `KnowledgeBase.Search`。

- **Tier 0（全部）**：HybridRetriever（BM25+Vector RRF）→ ContextExpander（LeafChunk 扩展 ParentChunk + 兄弟块，纯 DB 查询）
- **Tier 1+（FeatureDeepRAG 开启）**：QueryPlanner（LLM 分解）→ StructuredNavigator（目录导航）→ HybridRetriever → ContextExpander → 跨子查询 RRF 去重

`ContextExpander` 全 Tier 均启用（仅 DB 查询，无 LLM 开销），扩展结果封装为 `AugmentedContext`（含 ParentChunk / SectionPath / Provenance / 前后兄弟块）。`FeatureDeepRAG` 在 `feature_gate.go` 注册，Tier 0 返回 false，Tier 1+ 返回 true。

### 2.6 KnowledgeGraph (知识图谱增强)

双模式检索：
- **LocalSearch**：query 实体 → findSimilarEntities（top-5）→ BFS Traverse（depth=2，每节点最多 20 出边，总节点 ≤200）→ collectChunks。BFS 路径取 max TaintLevel 为 SubgraphMaxTaint，≥TaintMedium 时 Provenance 显式携带供 M4/M11 门控。
- **GlobalSearch**：findBestCluster → 社区生成式摘要（CommunityGenerativeSummarizer，DeepSeek 后台预计算）→ 注入 M4 LLM prompt。StalenessGuard 检测 generated_at vs 实体 updated_at：有变化注入提示+追加未归档实体，>30% 变化触发后台集群重建。

### 2.7 GraphBuildPipeline (知识图谱构建)

EntityExtractor/RelationExtractor/CrossDocumentLinker/Clusterer 实现见 `pkg/swarm/graph_build.go`。

触发: 文档摄入后, Ingester 通过 Outbox 写 graph_build_task。GraphBuildWorker 由 M2 全局 Outbox Worker 消费（注册于 handler 映射，见 §3.2），不独立开启内部轮询 goroutine。Phase 1-5 完整流程见代码。

**DocFetcher 注入**: `EntityExtractor.Extract` 接收文档文本（非 docID），调用方通过 `GraphBuildPipeline.SetDocFetcher(DocFetcher)` 注入内容获取器；未注入时降级为以 docID 字符串作为占位文本执行规则提取。LLM 路径通过 `ProviderLLMClient` 适配，规则路径为词典匹配+正则模式。

**HybridRetrieverImpl 双路检索** (`pkg/swarm/knowledge/retriever.go`):
- Tier 0 (embedder=nil): FTS5 BM25 单路，按 rank 排序，TopK 截断。
- Tier 1+ (embedder 非 nil): FTS5 + Dense Vector 双路 RRF 融合（k=60，FTS5 权重 1.0，向量权重 0.8）。密集向量通过 `VectorEmbedder.Embed` 计算查询向量后，与 `rag_chunks.embedding` 列余弦相似度排序；无 embedding 的 chunk 跳过（幂等）。`NewHybridRetrieverWithEmbedder` 注入 embedder，`NewHybridRetriever` 保持 Tier 0 默认。

**CommunityGenerativeSummarizer**（默认启用 DeepSeek 后台生成管道，不再受限于抽取式）:
1. **社区检测**: Leiden/Louvain 算法划分社区 (`gonum/graph/community`)
2. **PageRank**: 计算社区内实体中心度 → Top-5 高中心度实体
3. **TextRank**: 计算关联文本片段的句子中心度 → Top-3 关键文本片段
4. **GraphWriter 实体消歧**（纯内存处理，非独立物理写入器）: 写入语义图前对 entity name + type 做余弦相似度匹配 → 同一实体合并（跨名称语义去重，如 "DeepSeek-V3" 与 "DeepSeek V3"）→ 规范化 name 后构造 `MutationIntent`（Table=entities/relations，**Op=OpUpsert**）→ `[MutationBus].Submit` → M2 DatabaseWriter 单一 goroutine 串行物理落盘。所有图写入（M5 UpsertFact + M10 GraphBuildPipeline + M9 Consolidation）统一走此路径，不绕过 M2 单写者不变量（M2 §2.3 [HE-Rule-6]）

  **并发消歧 DDL 约束**：entities 表有 `UNIQUE(name, type)` 约束（DDL 见 `internal/protocol/schema/004_semantic_memory.sql`），GraphWriter 使用 ON CONFLICT DO UPDATE 幂等 Upsert，并发安全下推至 DB 约束；in-memory cosine 相似度检查仅用于 name 规范化（跨名称语义合并），非并发 guard。
5. **拼接存储**: `"Community entities: {e1, e2, e3}. Key context: {fragment1}. {fragment2}."` → `community_extractive_summary` 表
6. **延迟**: 纯 Go，<5ms，零外部依赖

**GraphRAG LLM 调用预算释放**:
- 每日 LLM 调用限制解除: 得益于 DeepSeek 的极低成本，Tier 1+ 原每日 500 次硬上限被彻底移除，全天候全量并发执行实体和关系提取。
- GraphBuildWorker: 不再因财务开销而强制阻塞 graph_build_task，任务可充分利用空闲算力高速消费。
- 优先级: 优先处理用户最近 24h 活跃检索的知识库文档的图谱构建，48h+ 未访问的文档降级为低优先级。
- Global Search: 默认启用高质量的后台 LLM 生成式摘要（Generative Summarization），废弃原“为了省钱而被迫采用的抽取式摘要”方案。

### 2.8 ConceptSynthesizer (跨文档合成)

触发门控：DocCount>20 且 Type 不在白名单（"API"/"ConfigParam"/"BusinessRule"/"DataType"）时跳过，防 LLM 洪峰。处理流程：① AggregateContext 从 CrossDocLinks 取相关 ParentChunk；② ContradictionDetection LLM 提取 key claims 跨文档对比；③ EvolutionDetection 按版本排序识别变更；④ LLM 合成 ~200 tokens CrossDocumentSummary（定义+共识+矛盾+演进）。输出 Taint 取 contexts[] max 并设 Floor-Medium，M4 门控禁止 TaintHigh 进入 instruction slot。

---

## 3. 增量索引

### 3.1 IncrementalIndexer

`FileTracker` 维护 source_uri → FileState（ContentHash/ModTime/LastIndexed/DocVersion/ChunkIDs）映射。Sync 主循环对每个 source：Hash 匹配跳过；新文件走 ingestNew；Hash 变更且存在并发编辑写 `document_sync_conflicts` 表 + 等待用户裁决；Hash 变更无并发编辑走 updateDocument（新 chunks + Tombstone 旧版 + 异步 Compaction Outbox 清理 SurrealDB-Core FTS）。`detectOrphans` 遍历 tracker 物理删除已删 source 的 chunks。

> ✅ **已修复**：IncrementalIndexer 完全缺失。`SyncScheduler` 能驱动 Ingest/Delete，但无 Hash 对比增量检测逻辑，无 `document_sync_conflicts` 表处理，无 Tombstone + Compaction 原子事务（P1 缺陷）。

### 3.2 Outbox 模式 — 复用 M2 全局引擎

M10 不实现独立的 OutboxWorker。本模块的 Outbox 表（`graph_build_task` / `summary_generation_task` / `compaction_task` / `cluster_rebuild_task`）位于 M2 SQLite 内，状态变更走 M2 MutationBus。消费循环复用 M2 全局 Outbox Worker（`pkg/substrate/outbox_worker.go`），M10 仅注册 handler:

`RegisterOutboxHandlers` 向 M2 全局 Outbox Worker 注册四类任务的 handler：`graph_build_task → GraphBuildPipeline.Run`、`summary_generation_task → IngestionSummarizer.Run`、`compaction_task → SurrealDB-CoreCompactor.Run`、`cluster_rebuild_task → ClusterRebuilder.Run`。

> ✅ **已修复**：代码中未找到 `RegisterOutboxHandlers` 调用，GraphBuildWorker 无法被 Outbox 驱动触发（P1 缺陷）。

写入路径: 共用 M2 outbox 表，新增 target_engine 取值 `m10_graph_build` / `m10_summary` / `m10_compaction` / `m10_cluster`。Ingester/GraphBuildPipeline 构造 `MutationIntent{Table:"outbox", Op:OpUpsert, ...}` → `[MutationBus].Submit(ctx, intent)` → DatabaseWriter 单写者串行化 → Outbox Worker 轮询消费。

与 M2 §2.5 全局 Outbox 的关系: M10 的 Outbox 任务通过 target_engine 维度区分，与 M2 跨引擎投影共享同一 outbox 表和同一 Outbox Worker 消费循环。写入和消费均走 M2 的 MutationBus + DatabaseWriter 统一基础设施，不绕过单写者约束。

---

## 4. 来源追踪

`ChunkProvenance` 携带来源完整元数据（SourceURI/SourceType/DocVersion/ChunkSeqIndex/AuthorityTier/IngestedAt/DocModifiedAt/EmbeddingModel/ChunkerVersion/IngestionRunID/ContentHash/ParentHash/ValidFrom/ValidUntil）。DDL NOT NULL 约束强制执行（inv_M10_03）。

`AuthorityTier`：1=官方/受信 > 2=社区/受信作者 > 3=公共知识库 > 4=用户上传/未验证。`RetrieveWithAuthority` 以 `RelevanceScore × authorityMultiplier`（Tier1×1.2 / Tier2×1.0 / Tier3×0.8 / Tier4×0.5）加权后过滤 minTier。

### 4.1 `[CitationValidator]` — D6 集成接口

> M11 [FactualityGuard] D6 防线（M11 §6.5）通过本接口核验 LLM 输出的引用真实性。

`CitationValidator.Validate` 执行三项全确定性校验（零 LLM，延迟 <20ms）：① chunk 存在性（ChunkIDs 校验 + ContentHash 未变更）；② 主张-证据 BM25 lexical match（70% 关键 token 命中为 valid）；③ 时效性（含时间限定词时检查 ValidUntil/IngestedAt）。返回 `ValidationResult{Valid, MissingTokens[], StaleChunks[], Confidence}`。

调用方: M11 FactualityGuard 抽样调用；M5 HybridRetriever 在 Read Path 末端可选调用（高优任务）。
延迟预算: <20ms (全确定性，零 LLM)。

### 4.2 Knowledge Conflict 仲裁

> 多源信息冲突时（如不同知识插件返回矛盾事实），需显式仲裁，不可静默选取。

**触发**: KnowledgeBase.Search 召回多 chunk，关键事实 token（数字/日期/实体属性）冲突。

**仲裁规则**（按优先级）:
1. **AuthorityTier 高者胜**: Tier1 (官方) > Tier2 (社区) > Tier3 > Tier4
2. **时效性优先**: 同 AuthorityTier 内，`IngestedAt` 更新者胜（ValidUntil 已过期者排除）
3. **多数共识**: 时效相同时，相同主张的 chunk 数 ≥3 → 接受多数；< 3 → 标记 `[KnowledgeConflict]` 不裁决，向 Agent 返回**全部**冲突候选 + 来源标签
4. **不可仲裁**: 三条均不成立 → `ErrKnowledgeConflictUnresolved` + M3 metric + Agent 选择 [ESCALATE] HITL 或 fallback 至最低风险默认

**输出**: KnowledgeBase.Search 返回结构含 `ConflictMarkers[]`（被仲裁淘汰的候选 + 淘汰原因），供 [CitationValidator] 和 [FactualityGuard] 审计追溯。

**约束**:
- 仲裁逻辑全确定性（零 LLM），延迟 <5ms
- ReplayMode 下重放仲裁过程一致（依赖 AuthorityTier + IngestedAt 单调性）

---

## 5. Reranking

### 5.1 SurrealDB-Core BM25 Reranker

BM25Reranker (接口定义见 `pkg/substrate/`，M5/M10 共享): k1=1.2, b=0.75。FinalScore=RRF融合分×0.7+BM25精确分×0.3。实现: SurrealDB-Core FFI (<5ms)。M10 通过接口注入，不直接依赖 SurrealDB-Core。

Tokenization: SurrealDB-Core + jieba-rs / lindera 多语言分词器；M3 启动期检测系统 locale，自动选择默认分词器。

方案选型：SurrealDB-Core BM25 FFI（<5ms，P0 采用）；Late-Interaction ColBERT-style（<20ms，研究方向）；ONNX / Python sidecar（因延迟/包体积不采用）。

### 5.2 Late-Interaction (研究方向)

暂不采用 Late-Interaction——Go 生态无成熟 BPE/WordPiece tokenizer。当前已实现基于 SurrealDB-Core（Rust FFI via purego）的单向量 KNN + BM25 的 RRF (Reciprocal Rank Fusion) 混合检索机制，满足生产环境需求。

---

## 6. 检索质量评估

Recall@K = hitCount/len(ExpectedChunks) vs MinRecall。完整Eval Harness集成见M12

---

## 7. 数据流闭环

`Knowledge Plugins → Document → 结构解析 → 文档树+父子双存+多级摘要 → 结构化导航 → 内容检索 → 上下文展开 → Agent 推理`

逐模块契约见 §10。

---

## 9. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| 知识插件 (Plugin) 不可达 | dead_letter + WARN + metric++ | 下次 doSync 重试 |
| Embedding 维度变更 (M1 Embedder 切换) | 路由切换至对应维度索引表（index_remote_4096 或 index_local_384），后台静默回填增量 | 新维度表就绪即可切换 |
| GraphRAG 实体数超 Tier 0 上限 (50K) | 拒绝摄入新文档 + polaris status 提示清理/升级 | 用户删除旧文档或升级 Tier |
| GraphRAG CPU/内存资源层往上触发 M9 BackgroundTaskScheduler 异常（非财务日预算） | 跳过剩余 graph_build_task + GlobalSearch 降级纯 Leiden 社区检测 | 资源释放后下一个窗口期自动恢复 |
| Chunk 检索超时 (>200ms) | 仅返回 BM25 结果 (跳过重排) | — |
| SurrealDB-Core FFI crash | 降级 SQLite FTS5 | 进程重启后恢复 SurrealDB-Core |

与 OSMemoryGuard 协同: L1 预警 → 限制插件并发摄入 / L2 紧急 → 暂停非关键数据源插件 / L3 临界 → 冻结全部摄入。


## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m10_kb`。

## 10. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces.go` + `types.go`。本表仅列依赖方向 + 一句话语义 + 锚点。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M10→M1 | Embedding API + LLM Budget Pool | chunk/query/summary 向量化 + 摘要/查询规划/实体提取。M1 §5, §6 |
| M10→M2 | Store 接口 + Outbox Worker | doc_nodes/entities/relations 三层索引；Outbox 共用。M2 §1, §2.5 |
| M10→M5 | HybridRetriever 共享引擎 | `pkg/substrate/hybrid_retrieve.go`；source_type 区分 kb_doc/kb_code vs episodic/semantic。M5 §7, M10 §2.2 |
| M10→M11 | Taint 初始打标 + SafeDialer | Scheduler 打标；BFS SubgraphMaxTaint 门控。M11 §2.4 |
| M4→M10 | StructuredNavigator → HybridRetriever → ContextExpander | LLM_fill 前的检索注入。M4 §3 |
| M9→M10 | 退化触发 → 增量重嵌入 + 摘要重生成 | 检索质量驱动的自演化。M9 §2.4 |
| Schema | HybridRetriever / SearchScope / RetrievalConfig / ScoredFragment | `internal/protocol/interfaces.go`, `types.go` |
| 全局字典 | HybridRetriever / RRF / BFS-Traverse / Spreading-Activation | 00-Global-Dictionary §9-bis |
| DDL | 001_events（doc_nodes 投影）、004_semantic_memory（图存储）| `internal/protocol/schema/` |
| 时序图 | Taint Tracking 全链路 | DIAGRAMS.md#taint-tracking |

---

## 11. 实现状态与 2026 研究对照

### 实现记录

| 组件 | 文件 | 状态 |
|------|------|------|
| ObsidianConnector + SyncScheduler | `pkg/swarm/knowledge/` | ✅ 已实现，Tier 0 内置 Connector |
| HybridRetrieverImpl（FTS5 + Vector RRF） | `pkg/swarm/knowledge/retriever.go` | ✅ 已实现；Tier 0 纯 FTS5，Tier 1+ 双路 RRF |
| ConflictArbiter（三级仲裁） | `pkg/swarm/knowledge/` | ✅ 已实现 |
| GraphBuildPipeline Phase 1-5 + ConceptSynthesizer | `pkg/swarm/graph_build.go` | ✅ 已实现 |
| PipelineImpl 分块策略 | `pkg/swarm/knowledge/chunker.go` | ✅ 已实现：`DefaultChunker` 按 sourceType 路由 `MarkdownChunker`（标题边界）、`CodeChunker`（func/class 边界）、`PlainTextChunker`（双换行 Fallback） |
| StructuredNavigator + QueryPlanner | `pkg/swarm/knowledge/rag_impl.go` | ✅ 已实现 |
| IncrementalIndexer（Hash 对比增量检测） | — | ✅ 已修复 |
| Outbox RegisterOutboxHandlers | — | ✅ 已修复：GraphBuildWorker 无法被 Outbox 驱动 |
| 向量检索 ANN 索引 | — | ⚠️ 当前为全表线性扫描（O(N) 余弦计算），无超时保护 |

### 引入计划

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **Path-Constrained Graph Search** | arXiv:2511.18313, 2025 | 图检索路径加关系类型约束，防止 BFS 跨越无意义边导致语义漂移（与 M5 §7.3 共享引擎，同步改动） | `pkg/substrate/hybrid_retrieve.go` | P2 |
