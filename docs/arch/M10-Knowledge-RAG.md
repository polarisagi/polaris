# 模块 10: Knowledge & RAG

> 消费 `[Storage-SQLite]` + `[Storage-SurrealDB-Core]`，非独立存储 | Hybrid Search + GraphRAG | 增量索引 | 来源追踪
> Go 检索流水线 + GraphRAG，Rust SurrealDB-Core FFI 侧车
> `[Code-Package-Mapping]`: pkg/swarm/ | `[Module-Topology]`: M10 L2 | `[HE-Rule-5]` `[HE-Rule-6]`
> **§跳读**: 0-bis:7 职责 / 0-ter:20 不变量速查 / 1:33 摄入 / 2:119 检索 / 3:229 增量索引 / 4:261 来源追踪 / 5:317 Reranking / 6:333 检索质量 / 7:339 数据流闭环 / 9:349 (SOFT)降级 / 10:367 跨模块契约
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
| inv_M10_05 | GraphRAG LLM 调用受日预算硬上限（Tier 0 上限见 `spec/state.yaml §m10_kb.graphrag_llm_call_daily_budget_ht0`）——超限跳过 graph_build_task | M10 §2.7 预算上限 |
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

**DocTree 持久化**: `PipelineImpl.Ingest` 将文档分块写入 `rag_chunks`（FTS5 支持）的同时，将 DocTree 结构序列化为 JSON 写入 `rag_docs` 表（uri 为主键，ON CONFLICT DO UPDATE 幂等）。

**可观测性**: 同步延迟 >1800s → ALERT + 暂停非关键知识源。

### 1.3 文档树数据结构

DocNode/LeafChunk/ParentChunk/Chunk 类型定义见 `pkg/swarm/knowledge/rag.go`。DocTree 在 `rag_docs` 表持久化（tree_json 列）。

### 1.4 结构解析 + 父子双存

DocumentParser: Parse(ctx, raw []byte, sourceURI string) → 根节点(NodeType=document), Children递归包含完整树

解析策略: Markdown→goldmark AST, PDF→pdfcpu布局, 代码→tree-sitter AST函数/类, Web→goquery+readability, 纯文本→空行+缩进启发式

Ingester.ParseAndBuildTree:
  1. 按 SourceType 选择解析器
  2. parser.Parse → DocNode 根节点
  3. buildChunks: 生成 LeafChunks + ParentChunks
  4. goroutine generateSummaries (后台 LLM)
  5. extractStructuredContent 提取表格 schema/代码块元数据

Ingester.buildChunks:
  1. walkTree 递归,按 NodeType 分支: Paragraph/Table/CodeBlock→splitToLeafChunks(~256 tokens)→独立embed→buildContextualContent→ParentChunk; 非叶→embedding=aggregateChildEmbeddings
  2. ParentChunk Content=SectionPath+前同级TopicSentence+节点完整内容
  3. ParentChunk Embedding=weightedAverageEmbedding(子LeafChunks)

### 1.5 嵌入维度变更 (三相渐进恢复)

M1 Embedder 模型切换致维度变更时，禁止全量同步重嵌 (`[Tier-0-Limit]` 8GB下不可行)。

| 阶段 | 触发 | 操作 | 延迟 |
|------|------|------|------|
| Phase 1: 双索引无缝切换 | Embedder.Dimension() 变更 | SurrealDB-Core 内维护双表 (index_remote_4096 / index_local_384)。切换模型时将检索请求路由至对应维度的表。**禁止永久降级 BM25**。 | <1ms |
| Phase 2: 优先级热恢复 | Phase 1 后 | 优先级队列重嵌 Top 50 Chunk: 0.4×M5 WorkingMemory引用频次 + 0.3×DurativeGroup活跃度 + 0.2×7天查询频率 + 0.1×访问时间倒数。每Chunk 2s超时，原子替换 | ~25-75s |
| Phase 3: 全量回填 | CPU idle>70% + 空闲内存>500MB | 后台单线程, 5文档/批+1s冷却 | ~5.5h (10K docs) |
| BM25 永久降级 | >30天未访问 | 不重嵌，永久BM25 | 0 |

跨模块: M2 OnlineReindexer 检测维度变更, M1 Embedder.Init() 暴露 Dimension(), M10 通过 `internal/config/runtime.go` embeddingDim atomic 获取, M5 ActiveDocumentTracker 提供文档热度

### 1.6 多级摘要生成

文档(~200 tokens) → 章节(~100 tokens) → 段落(~20 tokens)。三级树状预计算。

IngestionSummarizationConfig: MaxDocsPerHour(100), MaxTokensPerDoc(700: 段落≤30+章节≤100+文档≤200), RateLimiter(token bucket)

Ingester.generateSummaries:
  1. RateLimiter.Wait, 超限→跳过+slog.Warn
  2. 段落级(≤30 tokens): 优先取首句(len 10-120, 零LLM), 否则LLM生成
  3. 章节级(~100 tokens): 子节点主题句拼接→LLM
  4. 文档级(~200 tokens): 章节摘要+标题→LLM
  5. 三级共用maxTokensPerDoc预算, 耗尽后续跳过

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

4 级流水线:
  1. 三路并行宽召回(限定scope子树): errgroup BM25.SearchInScope(3×50) + denseVec.SearchInScope + graphDB.TraverseInScope(depth=2); 部分路失败→slog.Warn+继续
  2. RRF融合合并三路
  3. 重排: top RerankTopM(50)→SurrealDB-CoreReranker.Rerank
  4. 截断: FinalTopK(5)

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

  1. query→embed→summaryIndex.Search(top-20 摘要候选)
  2. 评分: 章节级命中×1.2, 文档级×1.0
  3. selectBestScope: 优先章节级
  4. RelevanceScore<0.5→fallback文档根节点(全文档搜索)

### 2.4 QueryPlanner

简单查询(<30 tokens)→跳过。复杂查询→LLM分解2-5子查询。QueryPlan{OriginalQuery, SubQueries[]{Query,TargetScope,Weight}, MergeStrategy(concat|deduplicate|interleave)}

### 2.5 KnowledgeBase.Search (完整入口)

  0. QueryPlanner.Plan
  对各SubQuery: 1. StructuredNavigator.Navigate→2. HybridRetriever.Retrieve→3. ContextExpander.Expand
  4. mergeAndRank: 跨子查询去重+RRF+权重排序

ContextExpander.Expand: LeafChunk.NodeID→DocNode→AugmentedContext{Content=ParentChunk, Location=SectionPath, Provenance, PrevSiblingContent, NextSiblingContent}

### 2.6 KnowledgeGraph (知识图谱增强)

双模式检索:
  LocalSearch: query实体→findSimilarEntities(top-5)→bfsTraverse(depth=2, 每节点最多20出边, 总节点≤200)→collectChunks
    `[Taint-Prop]`: BFS路径取max TaintLevel→SubgraphMaxTaint。≥`[Taint-Medium]`→Provenance显式携带，供M4/M11门控(TaintHigh实体仅允许data slot)
  GlobalSearch: findBestCluster → 取社区抽取式摘要（零 LLM，见 §2.7 CommunityExtractiveSummarizer）→ 注入 M4 LLM prompt 实现**延迟理解（Lazy Generation）**——将开销从预计算 O(N) 转移到读时 O(1)，仅在查询命中时让 LLM 理解摘要
    StalenessGuard: 对比摘要generated_at vs 实体updated_at max。delta>0→注入提示+追加未归档实体; delta>30%→触发后台集群重建

### 2.7 GraphBuildPipeline (知识图谱构建)

EntityExtractor/RelationExtractor/CrossDocumentLinker/Clusterer 实现见 `pkg/swarm/graph_build.go`。

触发: 文档摄入后, Ingester 通过 Outbox 写 graph_build_task。GraphBuildWorker 每5s轮询。Phase 1-5 完整流程见代码。

**DocFetcher 注入**: `EntityExtractor.Extract` 接收文档文本（非 docID），调用方通过 `GraphBuildPipeline.SetDocFetcher(DocFetcher)` 注入内容获取器；未注入时降级为以 docID 字符串作为占位文本执行规则提取。LLM 路径通过 `ProviderLLMClient` 适配，规则路径为词典匹配+正则模式。

**HybridRetrieverImpl 双路检索** (`pkg/swarm/knowledge/retriever.go`):
- Tier 0 (embedder=nil): FTS5 BM25 单路，按 rank 排序，TopK 截断。
- Tier 1+ (embedder 非 nil): FTS5 + Dense Vector 双路 RRF 融合（k=60，FTS5 权重 1.0，向量权重 0.8）。密集向量通过 `VectorEmbedder.Embed` 计算查询向量后，与 `rag_chunks.embedding` 列余弦相似度排序；无 embedding 的 chunk 跳过（幂等）。`NewHybridRetrieverWithEmbedder` 注入 embedder，`NewHybridRetriever` 保持 Tier 0 默认。

**CommunityExtractiveSummarizer**（纯 Go 确定性管道，零 LLM 调用）:
1. **社区检测**: Leiden/Louvain 算法划分社区 (`gonum/graph/community`)
2. **PageRank**: 计算社区内实体中心度 → Top-5 高中心度实体
3. **TextRank**: 计算关联文本片段的句子中心度 → Top-3 关键文本片段
4. **GraphWriter 实体消歧**（纯内存处理，非独立物理写入器）: 写入语义图前对 entity name + type 做余弦相似度匹配 → 同一实体合并（跨名称语义去重，如 "DeepSeek-V3" 与 "DeepSeek V3"）→ 规范化 name 后构造 `MutationIntent`（Table=entities/relations，**Op=OpUpsert**）→ `[MutationBus].Submit` → M2 DatabaseWriter 单一 goroutine 串行物理落盘。所有图写入（M5 UpsertFact + M10 GraphBuildPipeline + M9 Consolidation）统一走此路径，不绕过 M2 单写者不变量（M2 §2.3 [HE-Rule-6]）

  **并发消歧 DDL 约束（防重复实体竞态）**: entities 表须有 `UNIQUE(name, type)` 约束（DDL 见 `internal/protocol/schema/004_semantic_memory.sql`）。GraphWriter 构造的 OpUpsert 对应以下幂等 SQL，由 DatabaseWriter 在事务内执行：
  ```sql
  INSERT INTO entities (name, type, version, properties, updated_at)
  VALUES (?, ?, ?, ?, ?)
  ON CONFLICT(name, type) DO UPDATE SET
    version    = MAX(entities.version, EXCLUDED.version),
    properties = COALESCE(EXCLUDED.properties, entities.properties),
    updated_at = EXCLUDED.updated_at
  WHERE EXCLUDED.version >= entities.version
  ```
  两个并发 Worker 提交完全相同的实体时，第二条 SQL 的 ON CONFLICT 分支幂等执行（version 不增，updated_at 刷新），不产生重复记录。in-memory cosine 相似度检查的作用是 name 规范化（跨名称语义合并），而非并发安全 guard——并发安全完全下推至 DB UNIQUE 约束。
5. **拼接存储**: `"Community entities: {e1, e2, e3}. Key context: {fragment1}. {fragment2}."` → `community_extractive_summary` 表
6. **延迟**: 纯 Go，<5ms，零外部依赖

**GraphRAG LLM 调用预算上限**:
- 每日 LLM 调用上限: Tier 0 见 `spec/state.yaml §m10_kb.graphrag_llm_call_daily_budget_ht0`（单实体提取 + 关系提取），Tier 1+ = 500 次
- GraphBuildWorker 每次轮询前检查当日 GraphRAG LLM 调用计数 (`polaris_graphrag_llm_calls_daily` Counter)。已达上限 → 跳过本轮 graph_build_task + WARN + 任务保持 pending 状态 + 设置 `next_retry_at = 次日 00:00:00 UTC`（M2 Outbox Worker 主查询和迟提交补偿均检查 next_retry_at，跳过未来重试时间的记录，避免每 30s 无条件死循环扫描）
- 优先级: 优先处理用户最近 24h 活跃检索的知识库文档的图谱构建，48h+ 未访问的文档降级为低优先级
- Global Search 零 LLM 调用（抽取式摘要替代生成式摘要）

### 2.8 ConceptSynthesizer (跨文档合成)

触发门控: DocCount>maxDocsPerEntity(20)且Type不在白名单("API","ConfigParam","BusinessRule","DataType")→跳过(防高频通用实体LLM洪峰)

处理:
  1. AggregateContext: CrossDocLinks查DocID→取前maxDocsPerEntity(按relevance降序)→从docTree取各文档ParentChunk
  2. ContradictionDetection: LLM提取key claims→跨文档对比→同key不同value→Contradiction{EntityID,Key,ValueA/B,SourceDocA/B,Confidence}
  3. EvolutionDetection: 按DocVersion/IngestedAt排序→LLM识别新增/变更/废弃
  4. Synthesize: LLM生成CrossDocumentSummary(~200 tokens): 定义+共识+矛盾+演进

输出: ConceptSynthesis{EntityID, CrossDocumentSummary, Contradictions[], Evolutions[], TaintLevel(INT 0-4)}

合成Taint: contexts[]任一≥`[Taint-Medium]`→输出=max(contexts), 地板`[Taint-Floor-Medium]`。M4门控禁止TaintHigh进入instruction slot

---

## 3. 增量索引

### 3.1 IncrementalIndexer

FileState: SourceURI, ContentHash(SHA-256), ModTime, LastIndexed, DocVersion, ChunkIDs
FileTracker: source_uri→FileState

Sync主循环, 对每个source:
  - Hash匹配→跳过
  - 新文件→ingestNew
  - Hash变更+DocModifiedAt>LastSyncedAt+local hash≠prev hash→sync_conflict(写document_sync_conflicts表+WARN+Web UI; 用户选accept_remote/accept_local/merge)
  - Hash变更+无并发编辑→updateDocument: (1)新chunks带version+++parent_chunk_hash (2)单事务原子写: UpsertChunks(幂等键)+旧版标记archived(Tombstone)+更新tracker (3)异步scheduleCompaction清理SurrealDB-Core FTS旧索引(Outbox)
detectOrphans: 遍历tracker, 物理删除源已删chunks

### 3.2 Outbox 模式 — 复用 M2 全局引擎

M10 不实现独立的 OutboxWorker。本模块的 Outbox 表（`graph_build_task` / `summary_generation_task` / `compaction_task` / `cluster_rebuild_task`）位于 M2 SQLite 内，状态变更走 M2 MutationBus。消费循环复用 M2 全局 Outbox Worker（`pkg/substrate/outbox_worker.go`），M10 仅注册 handler:

```
RegisterOutboxHandlers:
  graph_build_task       → GraphBuildPipeline.Run
  summary_generation_task → IngestionSummarizer.Run
  compaction_task        → SurrealDB-CoreCompactor.Run
  cluster_rebuild_task   → ClusterRebuilder.Run
```

写入路径: 共用 M2 outbox 表，新增 target_engine 取值 `m10_graph_build` / `m10_summary` / `m10_compaction` / `m10_cluster`。Ingester/GraphBuildPipeline 构造 `MutationIntent{Table:"outbox", Op:OpUpsert, ...}` → `[MutationBus].Submit(ctx, intent)` → DatabaseWriter 单写者串行化 → Outbox Worker 轮询消费。

与 M2 §2.5 全局 Outbox 的关系: M10 的 Outbox 任务通过 target_engine 维度区分，与 M2 跨引擎投影共享同一 outbox 表和同一 Outbox Worker 消费循环。写入和消费均走 M2 的 MutationBus + DatabaseWriter 统一基础设施，不绕过单写者约束。

---

## 4. 来源追踪

ChunkProvenance (所有字段):
  SourceID(URI hash), SourceURI, SourceType
  DocVersion, ChunkSeqIndex
  AuthorityTier: 1=官方/受信, 2=社区/受信作者, 3=公共知识库, 4=用户上传/未验证
  IngestedAt, DocModifiedAt
  EmbeddingModel, ChunkerVersion, IngestionRunID
  ContentHash(SHA-256), ParentHash(上一版本chunk hash)
  ValidFrom, ValidUntil（**时效性窗口**，nullable，用于 §4.2 冲突仲裁）

AnswerCitation: Text, ChunkIDs[], Provenance[]

RetrieveWithAuthority:
  1. Retrieve获取候选
  2. RelevanceScore×authorityMultiplier(Tier1×1.2, Tier2×1.0, Tier3×0.8, Tier4×0.5)
  3. 过滤AuthorityTier>minTier

### 4.1 `[CitationValidator]` — D6 集成接口

> M11 [FactualityGuard] D6 防线（M11 §6.5）通过本接口核验 LLM 输出的引用真实性。

```
CitationValidator.Validate(citation AnswerCitation, claim string) → ValidationResult
  1. 引用 chunk 存在性: 所有 ChunkIDs 经 KnowledgeBase.GetChunk 校验存在 + ContentHash 未变更
  2. 主张-证据匹配:
     - 提取 claim 的关键 token（去停用词 + 命名实体识别）
     - BM25 lexical match 校验关键 token 在引用 chunk 文本中出现
     - 阈值: 70% 关键 token 命中 → valid，否则 invalid
  3. 时效性: claim 含时间限定词（"当前"/"最新"/"今年"）→ 检查 chunk.ValidUntil > now 或 IngestedAt 在最近 N 天内
  4. 返回 ValidationResult { Valid, MissingTokens[], StaleChunks[], Confidence }
```

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

BM25Reranker (接口定义见 `pkg/substrate/`，M5/M10 共享): k1=1.2, b=0.75。FinalScore=RRF融合分×0.7+BM25精确分×0.3。Tier 1+ 实现: SurrealDB-Core FFI (<5ms)；Tier 0 实现: SQLite FTS5 BM25 (`bm25()` 辅助函数, <3ms, 同公式)。HardwareProbe 启动时选择实现，M10 通过接口注入，不直接依赖 SurrealDB-Core。

Tokenization: Tier 0 → SQLite FTS5 + jieba 中文分词扩展（Go 纯实现）；Tier 1+ → SurrealDB-Core + jieba-rs / lindera 多语言分词器。M3 启动期检测系统 locale，自动选择默认分词器。

方案矩阵: SurrealDB-Core BM25 FFI(<5ms,P0) / Late-Interaction ColBERT-style(<20ms,研究方向MVP不用) / ONNX(~130ms,不采用,278M~500MB+) / Python sidecar(~150ms,不采用)

### 5.2 Late-Interaction (研究方向)

暂不采用 Late-Interaction——Go 生态无成熟 BPE/WordPiece tokenizer。目前 Tier 0 基线已实现基于 SurrealDB-Core（Rust FFI via purego，D-07 决议）的单向量 KNN + FTS5 BM25 的 RRF (Reciprocal Rank Fusion) 混合检索机制，满足生产环境需求。

---

## 6. 检索质量评估

Recall@K = hitCount/len(ExpectedChunks) vs MinRecall。完整Eval Harness集成见M12

---

## 7. 数据流闭环

```
Knowledge Plugins → Document → 结构解析 → 文档树+父子双存+多级摘要 → 结构化导航 → 内容检索 → 上下文展开 → Agent 推理
```

逐模块契约见 §10。

---

## 9. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| 知识插件 (Plugin) 不可达 | dead_letter + WARN + metric++ | 下次 doSync 重试 |
| Embedding 维度变更 (M1 Embedder 切换) | 路由切换至对应维度索引表（index_remote_4096 或 index_local_384），后台静默回填增量 | 新维度表就绪即可切换 |
| GraphRAG 实体数超 Tier 0 上限 (50K) | 拒绝摄入新文档 + polaris status 提示清理/升级 | 用户删除旧文档或升级 Tier |
| GraphRAG LLM 调用日预算超限 | 跳过剩余 graph_build_task + GlobalSearch 降级纯 Leiden 社区检测 | 次日 reset |
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
| GraphBuildPipeline Phase 5 | `pkg/swarm/graph_build.go:59` | **✅ 已完成** — 已实现 `ConceptSynthesizer` 核心逻辑，通过 LLM 或出现频次合成聚类概念，并调用 `SemanticMem` 持久化大概念实体及 `RELATED_TO` 关系边 |

### 引入计划

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **Path-Constrained Graph Search** | arXiv:2511.18313, 2025 | 图检索路径加关系类型约束，防止 BFS 跨越无意义边导致语义漂移（与 M5 §7.3 共享引擎，同步改动） | `pkg/substrate/hybrid_retrieve.go` | P2 |
