# ADR-0113: 知识库向量按模型版本收敛——KnowledgeReindexer 单一写入路径，删除无效线性降级

> 状态：Accepted ｜ 首次：2026-10-13 ｜ 最近复核：2026-10-13
> 相关模块：M10（`internal/knowledge`）/ M5（`internal/memory/retrieval`）/ M3（指标命名门控）/ `cmd/polaris`。前置：ADR-0109（嵌入器与同维换模型清空）、ADR-0099（Embedding 调度 High/Low 通道）、ADR-0112 决策八 #3。

## 现状缺陷（2026-10-13 代码核实）

| # | 缺陷 | 证据 |
|---|---|---|
| 1 | 文档摄入从不写向量：用户导入文档 / 连接器同步的知识块没有任何向量召回 | `DefaultIngestionPipeline.persistChunks` 只写 `rag_chunks` + BM25 统计（`HybridSearchEngine.AddDocument` 仅更新 CorpusStats）；全仓 `VecUpsert` 写知识向量的只有 `extension_librarian_handler.go` |
| 2 | `searchVectorFallback` 查询不存在的列 | `009_rag_chunks.sql` 无 `embedding` 列；调用即报错，被 `source.go` Warn 后吞掉 |
| 3 | `rag_chunks.embed_model_version` 恒为 `''` | `persistChunks` 写死 `""`；schema 注释称「OnlineReindexer 触发条件」不实（OnlineReindexer 只处理 `episodic_events`） |
| 4 | 情景记忆 SQLite 降级扫描不按模型版本过滤 | `retriever_helpers.go` `fetchVectorResultsFromSQL` 只过滤 `embedding IS NOT NULL`；换模型后重嵌完成前同维新旧向量混检 |
| 5 | 记忆 / 插件 / 知识共用一个 SurrealDB 向量索引 | 知识路径 `VecKNN(embedding, topK)` 未超取，非知识命中在 `fetchCognitiveHits` 被丢弃后召回不足 topK |

## 决策一：知识向量由 KnowledgeReindexer 单一路径写入

- 新增 `internal/knowledge` `KnowledgeReindexer`，形态对齐 `memory/retrieval.OnlineReindexer`：扫描 `rag_chunks WHERE deleted_at IS NULL AND embed_model_version != <当前版本>`（`''` 即未索引），分批取出后**先关闭 Rows** 再嵌入（R1.16），`VecUpsert(chunk.id, vec)` 成功后写回 `embed_model_version`。
- 当前版本取自嵌入器 `ModelVersion()`；为 `''`（引擎未就绪）时本轮跳过。嵌入走 Low 优先级通道（ADR-0099），不与交互检索争用。
- 首次索引、换模型后重建（ADR-0109 清空 SurrealDB 向量后版本必然不匹配）、失败重试共用这一条路径。摄入管线不直接嵌入，只在提交后非阻塞唤醒重嵌器（缩短首次可用延迟）。
- 连续嵌入失败按 OnlineReindexer 同款中止本批；失败行保持旧版本，下轮重试。全过程有指标与日志（ADR-0087 降级显式）。

## 决策二：删除知识库 SQLite 线性降级

- 删除 `searchVectorFallback`。SurrealDB 不可用或 `VecKNN` 失败时，知识向量路显式降级为空（Warn + `RecordKnowledgeReadFailure`），BM25 / 图谱两路照常。
- 理由：SQLite 不存知识向量，线性降级不可能工作；为它补存一份向量会造成双写与一致性负担，收益只覆盖「SurrealDB 不可用」这一异常态。

## 决策三：共享向量索引下的超取

知识路径 `VecKNN` 请求 `topK × 4`（上限 200），过滤出 `rag_chunks` 命中后截到 topK。按命名空间拆分向量索引属 Rust FFI 契约变更，暂不做；重新评估触发条件见下。

## 决策四：情景记忆降级扫描按版本过滤

`fetchVectorResultsFromSQL` 增加 `embed_model_version = <当前版本>` 过滤；版本为 `''` 时不走向量路。未重嵌的旧事件在收敛前只经 BM25 召回。

## 决策五：指标命名门控（落实 ADR-0112 决策八 #3）

`internal/lint` 新增 `Test_inv_MetricNaming`：OTel instrument 名须匹配 `polaris.<段>.<段>…`（小写、点号分段），Prometheus 原生指标 `Name` 须以 `polaris_` 开头。存量两处违例（`idle_evolution_tasks_total`、`polaris_dbwriter_panic`）随本 ADR 改名。

## 决策六：向量库清空即版本戳失效

`newEmbedModelSwitchHandler` 在 `VecClear` 成功后，同一流程内把 `rag_chunks` 与 `episodic_events` 的 `embed_model_version` 全部置 `''`，再记录新生效版本。不变量：**SurrealDB 向量被清空 ⇒ 所有版本戳失效**。否则 A→B→A 快速切换时，仍标着 A 的行会被判为「已是当前版本」而永不重写，向量永久丢失。置空失败按清空失败同等处理（不记录新版本，下次重试）。

## 决策七：单轮扫描游标防毒行饿死

KnowledgeReindexer 单轮内按 `rowid` 升序推进游标（`rowid > 上批最大 rowid`），本批失败行不再阻塞后续批次；失败行在下一轮从头扫描时重试。

## 决策五补记（落地实测）

按门控正则全仓扫描，除原列两处外另有 9 个 OTel instrument 为单段名（如 `polaris.goroutines`、`polaris.surprise_index`），一并改为两段以上（`polaris.runtime.goroutines`、`polaris.surprise.index` 等）。其中 `goroutines`、`memory_alloc_mb`、`agents_active`、`system1_bypass`、`dbwriter_panic` 的 Prometheus 导出名随之变化；`metrics_handler.go` 手写 `/metrics` 文本不在门控覆盖内。

## 重新评估触发条件

- 知识路径超取后命中率（过滤后条数 / topK）长期 < 0.5 → 评估向量索引按命名空间拆分（FFI 契约变更）。
- 知识块规模使重嵌器单轮扫描成为瓶颈 → 评估 `embed_model_version` 全量索引或分区扫描。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-10-13 | 首次。由 ADR-0112 决策八 #7 后续核实（M10 inv_M10_04「不得混检」现状）发现 |

## 引用代码

`internal/knowledge/rag_impl.go`、`internal/knowledge/retriever.go`、`internal/knowledge/source.go`、`internal/memory/retrieval/retriever_helpers.go`、`internal/memory/retrieval/online_reindexer.go`、`internal/protocol/schema/009_rag_chunks.sql`、`cmd/polaris/boot_knowledge.go`、`cmd/polaris/boot_embedding.go`
