# 模块 10: Knowledge & RAG（Retrieval-Augmented Generation，检索增强生成）

> 消费 `[Storage-SQLite]` + `[Storage-SurrealDB-Core]`，非独立存储 | Hybrid Search + GraphRAG | 增量索引 | 来源追踪
> Go 检索流水线 + GraphRAG，Rust SurrealDB-Core FFI 侧车
> `[Code-Package-Mapping]`: internal/swarm/| `[Module-Topology]`: M10 L2 | `[HE-Rule-5]` `[HE-Rule-6]`
<!-- §跳读: 0-bis:7 职责 / 0-ter:20 不变量速查 / 1:33 摄入 / 2:100 检索 / 3:175 增量索引 / 4:187 来源追踪 / 5:215 Reranking / 6:227 288 检索质量 / 7:233 294 数据流闭环 / 9:239 310 (SOFT)降级 / 10:257 328 跨模块契约 / 11:276 347 实现状态+2026研究对照 -->
## 0-bis. 职责边界

| M10 **是** | M10 **不是** |
|-----------|-------------|
| 外部文档摄入流水线（Plugin → Ingester → 层级文档树） | 记忆的读写管理（那是 M5） |
| 层级知识检索（结构化导航 + Hybrid 内容检索 + 上下文展开） | HybridRetriever 底层引擎实现（`internal/store/` 为 M5/M10 共享） |
| GraphRAG 知识图谱构建与双模式检索（Local/Global Search） | 图存储引擎（那是 M2 SQLite/邻接表或 SurrealDB-Core） |
| 多级预计算摘要生成（段落→章节→文档） | LLM（Large Language Model，大语言模型） 调用路由（那是 M1 Provider Router） |
| 增量索引 + 来源追踪（ChunkProvenance） | 嵌入模型选择（Embedding 调用走 M1 Provider Router） |
| 知识源同步调度（基于 Plugin 生态获取数据） | 第三方平台 API（Application Programming Interface，应用程序接口） 鉴权与网络请求（下放至独立 Plugin 沙箱隔离） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M10_01 | M10 不是独立存储——消费 M2 Store 接口，共享 M5 HybridRetriever 底层引擎 | 架构审计 |
| inv_M10_02 | 知识源拉取内容默认 taint=high——对于受信任的本地文件系统插件，可按策略降为 low | M11 Connector-Taint-Table |
| inv_M10_03 | 每 chunk 携带 lineage metadata——source_uri + doc_version + chunk_seq + content_hash | DDL（Data Definition Language，数据定义语言） NOT NULL 约束 |
| inv_M10_04 | 不同嵌入模型版本（`embed_model_version`）的向量不得混检——切模型/切档按版本重嵌（`rag_chunks.embed_model_version`、M5 `OnlineReindexer`），同维换模型亦然（ADR-0109 D5）。**现状**：检索路径（`internal/knowledge/retriever.go` `searchVectorFallback`）目前未按 `embed_model_version` 过滤（仅校验向量维度与查询一致），「不得混检」由重嵌收敛保证，而非检索期过滤 | M10 §1.5 |
| inv_M10_05 | GraphRAG LLM 取消严苛日预算限制（得益于 DeepSeek 的低成本）——全量生成高质量图谱 | M10 §2.7 预算释放 |
| inv_M10_06 | 主引擎 M10 彻底剥离出站网络权限——任何第三方 API 调用必须在沙箱化的 Plugin 进程中执行 | 代码审计（M10 内无 HTTP（HyperText Transfer Protocol，超文本传输协议） Client） |

---

## 1. 文档摄入流水线

### 1.1 层级索引管道 (6 阶段)

1. Knowledge Plugin (MCP（Model Context Protocol，模型上下文协议）) 拉取 → 统一 `Document` 格式
2. 结构解析 → Document Tree（标题层级 → 节点树）
3. 层级分块 + 父子双存
4. 多级摘要生成（后台 LLM，不阻塞摄入）
5. 元数据富化 + Embedding
6. 多引擎索引: `[Storage-SQLite]` doc_nodes + `[Storage-SurrealDB-Core]` 向量 + FTS5 全文

### 1.2 知识源摄入 (Plugin-driven + 直连 Connector)

**直连 Connector**（Tier 0 内置，`internal/knowledge/connector/`）:
- `ObsidianConnector`（`obsidian_connector.go`）：本地 Markdown 目录 fsnotify 监听（created/updated/deleted），List/Fetch/Watch 三接口，支持 YAML frontmatter。
- `NotionConnector`（`notion_connector.go`）：拉取 Notion 页面/数据库并映射为统一 `Document`。
- `MCPKnowledgeConnector`（`mcp_connector.go`）：消费声明 `capability: knowledge_provider` 的 MCP Server 资源。
- `ExtensionLibrarianHandler`（`extension_librarian_handler.go`）：对接扩展市场 Librarian 角色扩展。
- `SyncScheduler`（`sync_scheduler.go`）：消费 `KnowledgeConnector.Watch` 事件驱动 `KnowledgePipeline.Ingest/Delete`；启动先全量同步再切增量 Watch；防抖窗口 500ms，指数退避重试最多 3 次。

**Plugin-driven Ingestion（长期目标）**: 消费 `M13-bis Extension Registry` 中声明 `capability: knowledge_provider` 的 Plugin（MCP），外部知识获取下放至插件沙箱（L2/L3），零信任边界。

**DocTree 持久化**: `PipelineImpl.Ingest` 将分块写入 `rag_chunks`（FTS5），DocTree 序列化 JSON 写入 `rag_docs`（uri 主键，ON CONFLICT DO UPDATE 幂等）。同步延迟 >1800s → ALERT + 暂停非关键知识源。

### 1.3 文档树数据结构

DocNode/LeafChunk/ParentChunk/Chunk 类型见 `internal/knowledge/rag.go`；DocTree 持久化于 `rag_docs.tree_json`。

### 1.4 结构解析 + 父子双存

`DocumentParser` 按 SourceType 选解析策略（Markdown→goldmark AST、PDF→pdfcpu、代码→tree-sitter AST、Web→goquery+readability、纯文本→空行+缩进启发式），生成以 document 为根的 DocNode 树。`Ingester.ParseAndBuildTree` 分层生成 LeafChunk（~256 tokens 语义断点）和 ParentChunk（SectionPath + 前同级 TopicSentence + 完整内容），后台 goroutine 生成多级摘要，`extractStructuredContent` 提取表格 schema 与代码块元数据。

**当前实现**：`rag_impl.go` `chunkDocument` 语义边界切分：先按 `\n\n` 合并为 ParentChunk（≤1000 runes），再按中英文句末符切 LeafChunk（≤250 runes），无边界时兜底字符切；解析器见 `internal/knowledge/parsers.go`（goldmark/pdfcpu/纯 Go，代码文件用 CGO go-tree-sitter）；摘要见 §1.6。

### 1.5 嵌入维度变更 (三相渐进恢复)

Embedder 模型切换致向量不兼容时，禁止全量同步重嵌（`[Tier-0-Limit]` 8GB 下不可行）。进程内 ONNX 两档统一 512 维，切档只按 `embed_model_version` 重嵌，不同模型向量不可混检（ADR-0109 D2/D5）。

| 阶段 | 触发 | 操作 | 延迟 |
|------|------|------|------|
| Phase 1: 版本标记切换 | Embedder 模型/档位变更（`embed_model_version` 变化） | 无双索引、无 `vec_dim` 动态维度表：HNSW 维度恒 512 不重建，新写入向量带新 `embed_model_version`；仅当库内旧维度（如旧 2560）≠ 期望维度时启动期删旧向量与索引重建（ADR-0109 D5）。重嵌完成前 FTS/BM25 兜底，**禁止永久降级 BM25** | <1ms |
| Phase 2: 优先级热恢复 | Phase 1 后 | 优先级队列重嵌 Top 50 Chunk: 0.4×M5 WorkingMemory引用频次 + 0.3×DurativeGroup活跃度 + 0.2×7天查询频率 + 0.1×访问时间倒数；每 Chunk 2s 超时，原子替换 | ~25-75s |
| Phase 3: 全量回填 | CPU idle>70% + 空闲内存>500MB | 后台单线程，5 文档/批 + 1s 冷却 | ~5.5h (10K docs) |
| BM25 永久降级 | >30 天未访问 | 不重嵌，永久 BM25 | 0 |

跨模块: M5 OnlineReindexer（`internal/memory/retrieval/online_reindexer.go`，原地 UPDATE `episodic_events.embedding`，无 shadow table）检测变更；M1 Embedder.Init() 暴露 Dimension()；M10 经 `internal/config/runtime.go` embeddingDim atomic 获取；M5 ActiveDocumentTracker 提供文档热度。

### 1.6 多级摘要生成

三级树状预计算：段落（≤30 tokens，优先取首句零 LLM）→ 章节（~100 tokens，子主题拼接 LLM）→ 文档（~200 tokens，章节摘要+标题 LLM）。速率限制 MaxDocsPerHour=100、MaxTokensPerDoc=700，超限跳过。

**已实现**：`internal/knowledge/rag_summary_tree.go`（`buildSummaryTree` 编排 `fetchLeafChunks`/`summarizeText`/`insertSummaryChunk`/`generateSummaryLevels`），三级调用 `provider.Infer`，对应 `009_rag_chunks.sql` 的 `chunk_type='para_summary'/'chap_summary'/'doc_summary'`，由 `cmd/polaris/boot_knowledge.go` 接线。查询 leaf chunks 后须在 LLM 调用前显式 `rows.Close()`（R1.16），避免长耗时摘要占住 reader 连接池。另有 `internal/knowledge/graphrag/summary_gen_handler.go`（Outbox 异步处理器，监听 `rag_doc_summary_needed`，同样提前 `rows.Close()`）和 `community_summarizer.go`（`CommunityGenerativeSummarizer`，Leiden 社区摘要+关键词，见 §2.7）。

**摘要 Taint**: LLM 摘要最低 `[Taint-Floor-Medium]`；段落级继承源段落 TaintLevel，章节级取子节点 max，文档级取章节 max；`[Taint-Prop]` 只升不降。存储于 `rag_chunks.taint_level`（INT 0-4，schema 009），供 M4 TaintContextPropagation 门控。

### 1.7 内容感知分块

| 类型 | 叶节点 | 父节点 | 特殊处理 | 实现 |
|------|--------|--------|---------|------|
| Markdown | 按段落,~256 tokens,语义断点 | 完整段落+章节路径+前文 | 表格保留 schema,代码块保留语言 | `MarkdownChunker`（标题边界） |
| 代码 | 按 func/class 边界 | 完整函数体+文件/包路径 | import block 独立索引 | `CodeChunker`，tree-sitter AST（`chunker_cgo.go` `//go:build cgo` / `chunker_nocgo.go`，`CGO_ENABLED=0` 自动降级字符串匹配；CGO 依赖为 ADR-0011 受限例外，`go.mod` 须用 `CGO_ENABLED=1 go mod tidy` 维护） |
| PDF | 布局感知段落+表格单独提 | 完整段落+章节标题+页码 | 图片提取 alt text | 🟡 `parsers.go` `PDFChunker` 用 pdfcpu 提取文本后交 `PlainTextChunker`，版式感知（表格/标题结构）未实现 |
| Web | main content 段落,~256 tokens | 完整 section/article | 去除导航/广告/页脚 | `PlainTextChunker`（双换行边界） |
| 对话 | 按 turn 切分 | 前后 2 turn 完整上下文 | 标注 speaker | `PlainTextChunker` |

---

## 2. 层级知识检索

### 2.1 三阶段结构化检索

1. **结构化导航**: query → embed → 摘要层搜索（文档级+章节级摘要向量）→ 锁定目标 DocNode
2. **内容检索**: 目标子树内 Hybrid Search（BM25+Dense+实体图）→ Top50 → LeafChunk
3. **上下文展开**: LeafChunk → ParentChunk（完整段落+章节路径+前文衔接+来源追踪）→ prompt context

**分数透传**（ADR-0105 决策十）：`KnowledgeBase.Search` 在 `AugmentedContext.Score` 返回检索器分，`fsmKnowledgeAdapter` 据此填充回合内 RAG 召回分（不再恒填 1.0）；召回侧以名次做 RRF 融合，不依赖绝对量纲。

### 2.2 HybridRetriever (内容层)

HybridRetrieverConfig: BM25Weight=0.3, VectorWeight=0.6, GraphWeight=0.1, RRF_K 见 `spec/state.yaml §m5_memory.rrf_k`（M5/M10 共享）, OversampleN=3, RerankTopM=50, FinalTopK=5

**5 级流水线**: ① 三路并行宽召回（BM25 + Dense Vector + Graph Traverse，限定 scope 子树，部分路失败降级继续）→ ② RRF 融合 → ③ SurrealDB-Core BM25 Reranker（Top RerankTopM=50）→ ④ CrossEncoder 精排（可选，consumer-side `protocol.Reranker` 注入，代码字段 `ragReranker`，未注入跳过）→ ⑤ FinalTopK 截断。

共享 `internal/store/` 底层 RRF+Rerank 引擎；三路检索器（BM25/DenseVec/GraphTraverser）依赖注入绑定各模块存储后端。支持 `AsOf` 历史快照查询（过滤 `valid_from`/`valid_until` 不满足的实体节点）。M5 检索 episodic_events+semantic_entities（scope=memory），M10 检索 doc_nodes（先导航再检索，scope=document_tree）。差异锁定在 RetrievalConfig：

| 参数 | M5 | M10 |
|------|-----|------|
| BM25Weight / VectorWeight / GraphWeight | 0.3 / 0.6 / 0.1 | 0.3 / 0.6 / 0.1 |
| OversampleN | 3 | 3 |
| FinalTopK | 10 | 5 |
| RerankTopM | 30 | 50 |
| RRF_K | 60 | 60 |

### 2.3 StructuredNavigator (目录层)

在 `rag_chunks_fts`（FTS5）搜索摘要块，取 BM25 rank 最高的 doc_id 定位目标文档；RelevanceScore 低于阈值 fallback 全局全文搜索（向量索引在 SurrealDB-Core，Navigator 本身用 FTS5）。实现 `internal/knowledge/`。**FeatureGate**：`FeatureDeepRAG`（Tier 0+，≥8GB，需 ≥1GB 空闲内存）；<8GB 退化为全文搜索。

### 2.4 QueryPlanner

简单查询（<30 tokens）跳过分解走单路检索；复杂查询 LLM 分解为 2-5 个子查询（TargetScope + Weight），支持 concat/deduplicate/interleave 合并。受 `FeatureDeepRAG` 门控（同 §2.3），<8GB 跳过分解。

### 2.5 KnowledgeBase.Search (完整入口)

实现 `internal/knowledge/`。
- **<8GB（FeatureDeepRAG 不可用）**：HybridRetriever（BM25+Vector RRF）→ ContextExpander（LeafChunk 扩展 ParentChunk + 兄弟块，纯 DB 查询）
- **≥8GB（FeatureDeepRAG 开启）**：QueryPlanner → StructuredNavigator → HybridRetriever → ContextExpander → 跨子查询 RRF 去重

ContextExpander 全 Tier 启用，结果封装为 AugmentedContext（ParentChunk / SectionPath / Provenance / 前后兄弟块）。`FeatureDeepRAG` 注册于 `internal/observability/`，MinTier=Tier0，≥8GB 且 ≥1GB 空闲返回 true。

### 2.6 KnowledgeGraph (知识图谱增强)

- **LocalSearch**：query 实体 → findSimilarEntities（top-5）→ BFS Traverse（depth=2，每节点最多 20 出边，总节点 ≤200）→ collectChunks。BFS 路径取 max TaintLevel 为 SubgraphMaxTaint，≥TaintMedium 时 Provenance 显式携带供 M4/M11 门控。
- **GlobalSearch**：findBestCluster → 社区生成式摘要（`CommunityGenerativeSummarizer`，后台预计算）→ 注入 M4 prompt。StalenessGuard 比对 generated_at 与实体 updated_at：有变化注入提示+追加未归档实体，>30% 变化触发后台集群重建。

### 2.7 GraphBuildPipeline (知识图谱构建)

EntityExtractor/RelationExtractor/CrossDocumentLinker/Clusterer 见 `internal/knowledge/graphrag/`（`build.go`/`entity.go`/`cluster.go`/`community_summarizer.go`/`writer.go`），GraphTraverser 在 `graph_traverser.go`。

- **与 M5 共享抽取**（[ADR-0083](./decisions/ADR-0083-bitemporal-knowledge-graph.md)，原 ADR-0077）：`GraphBuildPipeline` 同时作为 `SharedEntityExtractor` 被 M5 Consolidation 复用（Phase1/Phase2 抽取），消除重复 LLM 燃烧与实体表述漂移；检索期 Spreading Activation 联合 M5/M10 两侧种子实体（M05 §9）。
- **触发**: 文档摄入后 Ingester 经 Outbox 写 `graph_build_task`；GraphBuildWorker 由 M2 全局 Outbox Worker 消费（handler 映射，见 §3.2），不独立轮询。Phase 1-5 见代码。
- **落库**: 实体/关系统一走 `SemanticMemory.UpsertFact / UpsertRelation`（`build.go` persistGraph，与 M5 consolidation 同一入口）→ `[MutationBus]` → M2 DatabaseWriter 单写者串行落盘（M2 §2.3 [HE-Rule-6]）。`semantic_entities` 的 `UNIQUE(name, type)`（`004_semantic_memory.sql`）+ ON CONFLICT DO UPDATE 保证并发幂等。`GraphWriter`（`writer.go`）无生产接线，不得作为落库路径。
- **CC-2 ResourceBudget 接线**（ADR-0025 BUG-2）：`graphPipeline.WithBackgroundGate(...)` 须传 `budget.NewResourceBudget(sb.TBR, graphGuard, graphGate)`；禁止零值 `&budget.ResourceBudget{}`（内存降级与 TBR P95 维度失效，高压下图重建无法被抑制）。
- **DocFetcher 注入**: `EntityExtractor.Extract` 接收文档文本；调用方经 `GraphBuildPipeline.SetDocFetcher` 注入内容获取器，未注入时以 docID 字符串占位执行规则提取。LLM 路径经 `ProviderLLMClient`，规则路径为词典匹配+正则。
- **HybridRetrieverImpl 双路检索**（`internal/knowledge/retriever.go`）: embedder=nil → FTS5 BM25 单路；embedder 非 nil → FTS5 + Dense Vector 双路 RRF（k=60，FTS5 权重 1.0，向量权重 0.8），向量与 `rag_chunks.embedding` 余弦排序，无 embedding 的 chunk 跳过。`NewHybridRetrieverWithEmbedder` 注入 embedder。
- **CommunityGenerativeSummarizer**: Leiden/Louvain 社区检测（`gonum/graph/community`）→ PageRank 取社区 Top-5 中心实体 → TextRank 取 Top-3 关键片段 → LLM 生成式摘要（`community_extractive_summary` 表）。**现状**：`Clusterer.Cluster` 无生产调用方，待 FeatureGraphRAGFull 门控启用时重新接线（经 SemanticMemory 落库）。
- **预算**（inv_M10_05）：无每日 LLM 调用硬上限，`graph_build_task` 不因财务开销阻塞；优先处理最近 24h 活跃检索的知识库文档，48h+ 未访问降为低优先级。


### 2.7-bis 关系边双时态化 + AsOf 视图（ADR-0083）

`semantic_relations`（事实=边）与实体侧对齐双时态：`valid_from`/`valid_until`/`status`/`superseded_by`（事务时间=`created_at`/`updated_at`，有效时间=`valid_from`/`valid_until`）。表级 `UNIQUE(source_id, target_id, relation_type)` 改为部分索引 `uq_semantic_rel_active ... WHERE status='active'`，同一三元组可保留多条历史版本，仅约束活跃版本唯一。

**信念修正**：`UpsertRelation` 写入时若目标三元组已有活跃边且 `weight` 变化超过 `relation_weight_delta_threshold=0.2`（或 `properties` 不同）→ 旧边 `status='superseded'`/`valid_until=now`/`superseded_by=<新边id>`，插入新活跃边；否则原地 `UPDATE`。

**AsOf 视图**：`internal/knowledge/graphrag/temporal_view.go` 的 `AsOfFilter{At}.SQLWhere(alias)`：零值 → `status='active'`（走既有索引）；非零 → 时间区间过滤（"当时以为的事实"）。已接入 `GraphTraverser.fetchNeighbors` 全部 4 条邻居查询与 `findSeedEntities`；`CascadeInvalidator`/`cognitive_replayer` 下游读路径同步补 `status='active'` 过滤。`TemporalExpirer.ExpireStale` 同调用内追加对 `semantic_relations` 的到期扫描，不新增 ticker。

### 2.8 ConceptSynthesizer (跨文档合成)

触发门控：DocCount>20 且 Type 不在白名单（"API"/"ConfigParam"/"BusinessRule"/"DataType"）时跳过，防 LLM 洪峰。流程：① AggregateContext 从 CrossDocLinks 取相关 ParentChunk；② ContradictionDetection 提取 key claims 跨文档对比；③ EvolutionDetection 按版本排序识别变更；④ LLM 合成 ~200 tokens CrossDocumentSummary（定义+共识+矛盾+演进）。输出 Taint 取 contexts[] max 并设 Floor-Medium，M4 门控禁止 TaintHigh 进入 instruction slot。

---

## 3. 增量索引

### 3.1 IncrementalIndexer

`Ingest` 以 ContentHash 检测是否跳过重摄取（hash 不变直接返回缓存 DocTree）。`Delete` 软删除（Tombstone：设 `deleted_at`），`rag_docs` / `rag_chunks` 均含 `deleted_at` 列。`DetectOrphans` 孤儿片段扫描、`FileTracker` 与并发编辑冲突检测（`document_sync_conflicts`）为设计目标，代码未实现（需在 `ROADMAP.md` 登记后再恢复描述）。

### 3.2 Outbox 模式 — 复用 M2 全局引擎

M10 不实现独立 OutboxWorker，复用 M2 全局 Outbox Worker（`internal/store/`）。`Ingester` 摄取完成后经 `outboxWriter.Write` 写 `target_engine=rag_doc_ingested` 事件，由已注册 handler（`graph_build_handler.go`、`summary_gen_handler.go`）消费，触发 GraphBuild / Summary。Outbox 任务 target_engine：`m10_graph_build` / `m10_summary` / `m10_compaction` / `m10_cluster`。写入和消费均走 M2 MutationBus + DatabaseWriter，不绕过单写者约束。

---

## 4. 来源追踪

`ChunkProvenance` 携带 SourceURI/SourceType/DocVersion/ChunkSeqIndex/AuthorityTier/IngestedAt/DocModifiedAt/EmbeddingModel/ChunkerVersion/IngestionRunID/ContentHash/ParentHash/ValidFrom/ValidUntil；DDL NOT NULL 约束强制（inv_M10_03）。

`AuthorityTier`：1=官方/受信 > 2=社区/受信作者 > 3=公共知识库 > 4=用户上传/未验证。独立的 `RetrieveWithAuthority` 加权检索未实现，AuthorityTier 加权尚未接线到检索路径（仅用于 §4.2 仲裁）。

**`TaintBoundarySerializer` 防篡改封印（inv_M11_02）**：`rag_chunks.taint_hmac` 列（`internal/protocol/schema/009_rag_chunks.sql`）由 `TaintBoundarySerializer.Seal`（`internal/knowledge/taint_boundary.go`）写入时计算 `HMAC-SHA256(content+taint_level+taint_source)`；读取时 `Unseal` 重算比对，不匹配或缺失（空串）视为篡改/不可验证，触发 fail-closed 降级。

### 4.1 `[CitationValidator]` — D6 集成接口（内嵌于 FactualityGuard）

M11 [FactualityGuard] D6 防线（M11 §6.5）经本接口核验 LLM 输出引用真实性。`CitationValidator` 非独立结构体，逻辑内嵌于 `internal/security/guard/factuality_guard.go` 的 L1 CitationCheck：三项全确定性校验（零 LLM，<20ms）：① chunk 存在性（ChunkIDs + ContentHash 未变更）；② 主张-证据 BM25 lexical match（70% 关键 token 命中为 valid）；③ 时效性（含时间限定词时检查 ValidUntil/IngestedAt）。返回 `ValidationResult{Valid, MissingTokens[], StaleChunks[], Confidence}`。调用方：M11 FactualityGuard 抽样；M5 HybridRetriever 在 Read Path 末端可选调用（高优任务）。

### 4.2 Knowledge Conflict 仲裁

多源信息冲突时显式仲裁，不可静默选取。**触发**: `KnowledgeBase.Search` 召回多 chunk，关键事实 token（数字/日期/实体属性）冲突。

**仲裁规则**（按优先级，`internal/knowledge/conflict.go`）:
1. **AuthorityTier 高者胜**: Tier1 > Tier2 > Tier3 > Tier4
2. **时效性优先**: 同 Tier 内 `IngestedAt` 更新者胜（ValidUntil 已过期者排除）
3. **多数共识**: 时效相同时相同主张 chunk 数 ≥3 → 接受多数；<3 → 标记 `[KnowledgeConflict]` 不裁决，向 Agent 返回**全部**冲突候选 + 来源标签
4. **不可仲裁**: 三条均不成立 → **兜底选取最高优先级候选**（`Arbitrate` 返回 `consensusPick(recentGroup)` 或 `candidates[0]`，`ArbitrateChunks` 返回 `group[0]`），附 reason 标记供审计；**不抛错、不阻断检索**

> 裁决（DR-3-008）：文档随代码，不定义 `ErrKnowledgeConflictUnresolved`——仲裁位于检索热路径（全确定性、<5ms），抛错会让无关紧要的版本冲突使整条 RAG 检索失败；HITL 升阶由消费检索结果的 Agent 按下游任务风险决定（M13 §2.4）。

**输出**: `KnowledgeBase.Search` 返回 `ConflictMarkers[]`（被淘汰候选 + 淘汰原因），供 [CitationValidator] 和 [FactualityGuard] 审计。**约束**: 全确定性（零 LLM），<5ms；ReplayMode 下重放一致（依赖 AuthorityTier + IngestedAt 单调性）。

---

## 5. Reranking

### 5.1 SurrealDB-Core BM25 Reranker

BM25Reranker（接口见 `internal/`，M5/M10 共享）: k1=1.2, b=0.75。FinalScore=RRF融合分×0.7+BM25精确分×0.3。实现: SurrealDB-Core FFI（<5ms），M10 经接口注入，不直接依赖 SurrealDB-Core。分词：SurrealDB-Core + jieba-rs / lindera 多语言分词器，M3 启动期按系统 locale 自动选择。选型：SurrealDB-Core BM25 FFI（P0 采用）；Late-Interaction ColBERT-style（<20ms，研究方向）；ONNX / Python sidecar（延迟/包体积不采用）。

### 5.2 Late-Interaction (研究方向)

暂不采用 ONNX-ColBERT Late-Interaction（Go 生态无成熟 BPE/WordPiece tokenizer）；当前为 SurrealDB-Core（Rust FFI via purego）单向量 KNN + BM25 的 RRF 混合检索。`internal/store/search/reranker.go` 的 `ApproximateColBERTReranker`（n-gram 窗口 + 现有 Embedder 近似 MaxSim）未被 HybridRetriever 主链路引用（`cmd/polaris/` 零接线），是未挂载的孤立组件。

---

## 6. 检索质量评估

Recall@K = hitCount/len(ExpectedChunks) vs MinRecall。完整 Eval Harness 集成见 M12。

---

## 7. 数据流闭环

Knowledge Plugins → Document → 结构解析 → 文档树 + 父子双存 + 多级摘要 → 结构化导航 → 内容检索 → 上下文展开 → Agent 推理。逐模块契约见 §10。

---

## 9. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| 知识插件不可达 | dead_letter + WARN + metric++ | 下次 doSync 重试 |
| Embedding 模型/维度变更 | 按 `embed_model_version` 标记切换，后台低速重嵌增量，期间 FTS 兜底（§1.5） | 新索引就绪即切换 |
| GraphRAG 实体数超 Tier 0 上限 (50K) | 拒绝摄入新文档 + polaris status 提示清理/升级 | 用户删除旧文档或升级 Tier |
| GraphRAG CPU/内存资源层触发 M9 BackgroundTaskScheduler 异常 | 跳过剩余 graph_build_task + GlobalSearch 降级纯 Leiden 社区检测 | 资源释放后下一窗口期恢复 |
| Chunk 检索超时 (>200ms) | 仅返回 BM25 结果（跳过重排） | — |
| SurrealDB-Core FFI crash | 降级 SQLite FTS5 | 进程重启后恢复 |

与 OSMemoryGuard 协同: L1 预警 → 限制插件并发摄入 / L2 紧急 → 暂停非关键数据源插件 / L3 临界 → 冻结全部摄入。


## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m10_kb`。

## 10. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces_*.go`（按域拆分）+ `types.go`。本表仅列依赖方向 + 一句话语义 + 锚点。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M10→M1 | Embedding API + LLM Budget Pool | chunk/query/summary 向量化 + 摘要/查询规划/实体提取。M1 §5, §6 |
| M10→M2 | Store 接口 + Outbox Worker | doc_nodes/entities/relations 三层索引；Outbox 共用。M2 §1, §2.5 |
| M10→M5 | HybridRetriever 共享引擎 | `internal/store/`；source_type 区分 kb_doc/kb_code vs episodic/semantic。M5 §7, M10 §2.2 |
| M10→M11 | Taint 初始打标 + SafeDialer | Scheduler 打标；BFS SubgraphMaxTaint 门控。M11 §2.4 |
| M4→M10 | StructuredNavigator → HybridRetriever → ContextExpander | LLM_fill 前的检索注入。M4 §3 |
| M9→M10 | 退化触发 → 增量重嵌入 + 摘要重生成 | 检索质量驱动的自演化。M9 §2.4 |
| Schema | HybridRetriever / SearchScope / RetrievalConfig / ScoredFragment | `internal/protocol/interfaces_*.go`, `types.go` |
| 全局字典 | HybridRetriever / RRF / BFS-Traverse / Spreading-Activation | 00-Global-Dictionary §9-bis |
| DDL | 001_events（doc_nodes 投影）、004_semantic_memory（图存储）| `internal/protocol/schema/` |
| 时序图 | Taint Tracking 全链路 | DIAGRAMS.md#taint-tracking |

---

## 11. 实现状态与 2026 研究对照

主链路均已实现（`internal/knowledge/`）：ObsidianConnector + SyncScheduler（Tier 0 内置）、HybridRetrieverImpl（Tier 0 纯 FTS5，Tier 1+ 双路 RRF，全 Tier 接入 GraphTraverser，BM25×0.3+Vector×0.6+Graph×0.1）、GraphTraverser（BFS depth=2，每节点≤20边，总节点≤200，关联 chunk 按深度衰减；`TraverseChunksWithOptions(TraverseOptions)` 支持 RelationTypes 白名单 / Direction / MaxDepth）、ConflictArbiter、GraphBuildPipeline Phase 1-5 + ConceptSynthesizer、DefaultChunker（Markdown/Code/PlainText 路由）、StructuredNavigator + QueryPlanner、IncrementalIndexer（Hash 增量）、Outbox handler 注册。

向量检索 ANN：Tier 1+（有 SurrealDB-Core 认知存储）用 HNSW 索引做 `VecKNN`（`rust/substrate/src/surreal_store/mod.rs` + `internal/knowledge/retriever.go`）；仅 Tier 0 或 cognitive store 不可用时降级 `searchVectorFallback` 全表线性扫描（O(N)，5000 行上限）。

### 引入计划

| 研究 | 来源 | 核心机制 | 引入点 | 优先级 |
|------|------|---------|-------|-------|
| **Path-Constrained Graph Search** | arXiv:2511.18313, 2025 | 图检索路径加关系类型约束，防止 BFS 跨越无意义边导致语义漂移（与 M5 §7.3 共享引擎，同步改动） | `internal/store/` | P2 |
