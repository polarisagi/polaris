# ADR-0083: 知识图谱：M5↔M10 桥接与抽取统一、关系边双时态化与 AsOf 视图

> 状态：Accepted ｜ 首次：2026-07-23 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0074、ADR-0077 ｜ 模块：M5 `internal/memory/`、M10 `internal/knowledge/graphrag/`

## 旧引用映射

| 旧引用 | 新节 |
|---|---|
| 0074 决策（写入期去重 + 检索期联合种子） / 0077 决策一 | 决策一 |
| 0074 §3 抽取不合并（已推翻）/ 0077 决策二 | 决策二 |
| 0083 决策 1~6 | 决策三 |

## 决策一：M5↔M10 最小整合桥接，保留双轨（原 ADR-0074、原 ADR-0077 决策一）

- 拒绝"M5 完全并入 M10"物理合并（违背 Tier-0 SQLite / Tier-1+ 图存储分级，ADR-0033 不变）。
- 写入期去重：`GraphWriter.UpsertEntity`（`internal/knowledge/provider.go`）插入前查 `semantic_entities`，同名实体走信念修正而非重复写入；实体标记 `source_type='graphrag_ingest'`。
- 检索期联合种子：`SemanticMem` 的 Spreading Activation 种子纳入 `source_type='graphrag_ingest'`（`internal/memory/store/semantic_mem_query.go`）。注：注入 prompt 的召回端（`entityRecallable`，`cmd/polaris/adapters_recall.go`）排除该来源，因 RAG 来源已带正文（ADR-0105 决策五）。
- 范围仅实体去重 + 检索种子；两套管线的写入 API/生命周期/存储选型保持独立。

## 决策二：实体/关系抽取实现统一（原 ADR-0077 决策二，推翻原 ADR-0074 §3"抽取不共享"）

- 仅推翻"抽取实现是否共享"；决策一的桥接继续有效，物理合并仍被否决。
- `internal/memory/consolidation` 消费方接口 `SharedEntityExtractor`（R1.4）；`graphrag.GraphBuildPipeline.ExtractEntitiesAndRelations` 实现之（只包装 Phase1+Phase2，不含聚类/概念合成）。
- 三级降级链：共享实现 → 历史兼容 `llmExtract`（graphrag 未装配）→ `ruleExtract` 正则回退。`cmd/polaris/boot_knowledge.go` 装配；`FeatureGraphRAGFull` 未启用时 Tier-0 行为零回归。
- 动因：两条管线各自调 LLM 抽取，双份燃烧与措辞漂移（决策一的去重只对 exact-match 生效）。
- 反例：重提 M5 物理并入 M10；抽取共享引入聚类/概念合成。

## 决策三：关系边双时态化 + AsOf 视图（原 ADR-0083 决策 1~6）

1. **关系边双时态化**：`semantic_relations` 新增 `valid_from`/`valid_until`/`status`/`superseded_by`，语义与实体侧对齐（事务时间 = `created_at`/`updated_at`，有效时间 = `valid_from`/`valid_until`）。此前实体已有双时态（Zep/Graphiti 模式），关系边没有，导致只能表达"实体何时有效"而不能表达"事实何时有效"。
2. **唯一性改部分索引**：删除表级 `UNIQUE(source_id,target_id,relation_type)`，改 `uq_semantic_rel_active ... WHERE status='active'`——同一三元组可保留多个历史版本，只约束当前活跃版本唯一。依赖旧 `ON CONFLICT(source_id,target_id,relation_type)` 的语句必须同步改为部分索引形式（遗漏会 fail-fast 报约束错误）。
3. **信念修正**：`UpsertRelation` 遇已有活跃边且内容实质变化（`weight` 变化 >`relation_weight_delta_threshold`=0.2，或 `properties` 不同）→ 旧边置 `superseded`/`valid_until=now`/`superseded_by=<新边id>`，插入新活跃边；否则原地 UPDATE（避免版本链膨胀）。实现 `internal/memory/store/semantic_relation_temporal.go`。
4. **AsOf 视图**：`graphrag.AsOfFilter{At}.SQLWhere(alias)`（`temporal_view.go`）：零值 → `status='active'`（走 `idx_semantic_rel_status`）；非零 → `(valid_from IS NULL OR valid_from<=t) AND (valid_until IS NULL OR valid_until>t)`。接入 `GraphTraverser.fetchNeighbors` 的 4 条邻居查询与 `findSeedEntities`。AsOf 参数解析失败在 `memory_search` 侧以 `CodeInvalidInput` 硬失败，不静默退回当前视图。
5. **下游读路径按 status 过滤**：`CascadeInvalidator`（级联失效 CTE）与 `cognitive_replayer`（SurrealDB 冷回放）遍历 `semantic_relations` 必须显式 `WHERE status='active'`，否则历史版本边污染级联传播与回放。
6. **`TemporalExpirer.ExpireStale`** 同次调用追加对 `semantic_relations` 的到期扫描（`valid_until<=now AND status='active'` → `expired`），不新增 ticker。

- 代价：`UpsertRelation` 由单条 UPSERT 变为"读+判定+条件写"，多一次读延迟。
- 反例：用 `updated_at` 冒充有效时间；AsOf 扫全表再 Go 侧过滤（须走 `idx_semantic_rel_valid`/`idx_semantic_rel_status`）；关系边"次要证据累积/权重微调"走版本升级（版本链爆炸，应原地 UPDATE）。

## 已驳方案

| 方案 | 驳回理由 |
|---|---|
| 独立 `semantic_relations_history` 历史表 | 图遍历同时查当前+历史多一次 JOIN/UNION，Tier-0 SQLite 上代价翻倍；同表 status 分区已足够 |
| 表级 `UNIQUE(source_id,target_id,relation_type,status)` | 多条 `superseded` 版本 status 相同仍互撞，必须用部分索引只约束 `active` 切片 |
| AsOf 解析失败静默退回当前视图 | 调用方以为查历史实得当前，属静默错误 |
| M5 完全并入 M10（GD-13-002） | 改动过大，违背 Tier-0/Tier-1+ 存储分级 |

## 重新评估触发条件

- 重提 M5 物理并入 M10：须证明 Tier-0 SQLite / Tier-1+ 图存储分级已不再是约束（如 Tier-0 硬下限被取消），而非"两套管线维护麻烦"。
- 独立历史表：仅在实测证明同表 status 分区使查询计划劣化到不可接受时。
- 关系边次要证据累积/权重微调绕过阈值走版本升级前，先确认不会造成版本链爆炸。

## 沿革

| 日期 | 变更 | 原编号 |
|---|---|---|
| 2026-07-23 | 最小整合桥接（写入期去重 + 检索期联合种子），抽取实现不共享 | 0074 |
| 2026-07-25 | 推翻"抽取不共享"，引入 `SharedEntityExtractor`（D3，原 GD-13-003） | 0077 |
| 2026-07-28 | 0074 并入 0077 | — |
| 2026-08-02 | 关系边双时态化落地（阶段05 P-02） | 0083 |
| 2026-10-12 | 0077 并入本文；召回注入端排除 `graphrag_ingest` 一行订正 | — |

## 引用代码

- `internal/protocol/schema/004_semantic_memory.sql`（`uq_semantic_rel_active`、`idx_semantic_rel_valid/status`）
- `internal/memory/store/semantic_mem.go`、`internal/memory/store/semantic_relation_temporal.go`、`internal/memory/store/semantic_mem_query.go`
- `internal/knowledge/graphrag/temporal_view.go`、`internal/knowledge/graphrag/graph_traverser.go`、`internal/knowledge/graphrag/build.go`、`internal/knowledge/provider.go`
- `internal/memory/retrieval/cascade_invalidator.go`、`internal/memory/retrieval/cognitive_replayer.go`、`internal/memory/graph/temporal.go`
- `internal/memory/consolidation/consolidation.go`（`SharedEntityExtractor`）、`internal/memory/consolidation/consolidation_extract.go`、`cmd/polaris/boot_knowledge.go`
- `docs/arch/spec/state.yaml`（`relation_weight_delta_threshold`）
