# ADR-0077: M5↔M10 桥接与实体/关系抽取统一（推翻原 ADR-0074 §3 抽取不合并结论，含原 ADR-0074）

- **状态**: Accepted（已执行）| **日期**: 2026-07-23（推翻部分结论 2026-07-25，合并 2026-07-28）| **模块**: `internal/memory/` / `internal/memory/consolidation/` / `internal/knowledge/graphrag/`

## 决策一：最小整合桥接（原 ADR-0074，写入期去重+检索期联合种子部分继续有效）

拒绝 GD-13-002 提议的"M5 完全并入 M10"（改动过大，违背 Tier-0 SQLite / Tier-1+ 图存储分级架构）。采用最小整合桥接，保留双轨架构：

- **写入期去重**：`GraphWriter.UpsertEntity` 插入前查 `semantic_entities`，同名实体走信念修正而非重复写入；实体标记 `source_type='graphrag_ingest'`。
- **检索期联合种子**：`SemanticMem` 检索的 Spreading Activation 种子纳入 `source_type='graphrag_ingest'`，联合查询外部文档与 Agent 知识。
- **范围**：仅实体去重+检索种子统一，不做物理合并；两套管线写入 API/生命周期/存储选型保持独立。是对 [ADR-0033](./ADR-0033-memory-subsystem-scope.md) 的补充，不改变其分级选型策略。

## 决策二：实体/关系抽取实现统一（原决策，推翻决策一"抽取实现不共享"的结论）

M5 Episodic→Semantic 蒸馏管线与 M10 RAG 摄取管线此前各自独立调用 LLM 做实体/关系抽取，存在双份燃烧与措辞漂移（决策一的写入期去重只对 exact-match 生效）。**仅推翻"抽取实现是否共享"这一结论**，不推翻决策一已落地的写入期去重桥接与检索期联合种子（两者在存储物理分离前提下仍必要，继续保留）；也不做决策一已否决的"M5→M10 物理合并"。

`internal/memory/consolidation` 新增消费方接口 `SharedEntityExtractor`（R1.4），`graphrag.GraphBuildPipeline` 新增导出方法 `ExtractEntitiesAndRelations` 实现该接口（仅包装 Phase1+Phase2，不含聚类/概念合成）。三级降级链：共享实现 → 历史兼容 `llmExtract`（`graphrag` 未装配时）→ `ruleExtract` 正则回退。`cmd/polaris/boot_knowledge.go` 装配，`FeatureGraphRAGFull` 未启用时 Tier-0 行为零回归。

## 反例守护

拒绝重提"M5 完全并入 M10"物理合并方案——决策一的否决结论未被推翻。拒绝抽取共享实现引入聚类/概念合成——仅包装 Phase1+Phase2。

## 引用代码

`internal/memory/consolidation/consolidation_extract.go`、`internal/knowledge/graphrag/build.go`

> 2026-08-09 追记：重新评估触发条件——"M5 完全并入 M10"物理合并方案已被决策一
> 否决且决策二明确重申未推翻，重提须证明 Tier-0 SQLite / Tier-1+ 图存储分级
> 架构本身已不再是约束（如 Tier-0 硬下限被取消），而不是单纯"两套管线维护
> 麻烦"。
