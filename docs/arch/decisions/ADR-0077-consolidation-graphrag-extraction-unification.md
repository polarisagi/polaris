# ADR-0077: Consolidation 与 GraphRAG 实体/关系抽取合一（推翻 ADR-0074 §3 不合并结论）

## 状态
Accepted（已执行）

## 背景

`internal/memory/consolidation`（M5 Episodic→Semantic 蒸馏管线）与
`internal/knowledge/graphrag`（M10 RAG 文档摄取→知识图谱管线）此前各自维护
一套独立的 LLM 实体/关系抽取实现：

- `consolidation.llmExtract` 用 `entity_extraction.tmpl` Prompt 调用
  `p.summarizer.InferRaw`，解析会话 Episodic 事件文本。
- `graphrag.EntityExtractor`/`RelationExtractor` 用另一套 Prompt 调用 LLM，
  解析 RAG 文档文本。

ADR-0074（Accepted/Executed）已评估过完全合并方案（原始设计 GD-13-002/
GD-13-003），结论是"改动面过大，且违背 Tier-0 使用 SQLite、Tier-1+ 使用图
存储的分级架构设定"，因而只做了写入期实体去重桥接（`GraphWriter.UpsertEntity`
查重）与检索期联合种子（`source_type='graphrag_ingest'` 纳入 Spreading
Activation），明确声明"完整合并留待未来专项评估，不在本 ADR 范围内"。

本次复核（gemini-review-design.md GD-13-003 D3）重新评估该范围声明后判断：
两条管线各自独立调用 LLM 抽取同一类实体/关系，存在双份 LLM 燃烧与实体表述
漂移（同一实体在两条管线里可能被抽取为不同的 name/type 变体，即便有 ADR-0074
的写入期去重桥接，去重只对 exact-match 生效，无法消除"抽取措辞不一致"导致的
漏配）。用户在本次改动前被明确告知该方案与 ADR-0074 的既有结论冲突，选择
"仍然推翻 ADR-0074，做完全合并"，故有本 ADR。

## 决策

**范围界定：本 ADR 只推翻 ADR-0074 关于"抽取实现是否共享"的结论，不推翻
ADR-0074 已落地的写入期去重桥接与检索期联合种子——那两项在两套存储物理分离
（Tier-0 SQLite / Tier-1+ 图存储）的前提下仍然成立且必要，本次改动之后依旧
保留、不受影响。** 换言之，本 ADR 缩小口径为"合并抽取这一个 Stage"，而非
ADR-0074 讨论并否决的"M5→M10 物理合并"（后者涉及存储选型、生命周期管理、
Tier 分级等更大改动面，本 ADR 依旧不做）。

1. **共享抽取接口**：`internal/memory/consolidation` 新增消费方接口
   `SharedEntityExtractor`（R1.4：接口定义在使用方包内）：
   ```go
   type SharedEntityExtractor interface {
       ExtractEntitiesAndRelations(ctx context.Context, sourceID, text string) ([]*types.Entity, []*types.Relation, error)
   }
   ```
   `ConsolidationPipeline` 新增 `entityExtractor SharedEntityExtractor` 字段
   与 `WithEntityExtractor(ee SharedEntityExtractor) *ConsolidationPipeline`
   注入方法，与既有 `GraphEntityFetcher`（B2 桥接）同一模式。

2. **唯一实现来源**：`internal/knowledge/graphrag.GraphBuildPipeline` 新增
   导出方法 `ExtractEntitiesAndRelations(ctx, sourceID, text) ([]*Entity,
   []*Relation, error)`，仅包装既有 Phase1（`entityExtractor.Extract`）+
   Phase2（`relationExtractor.Extract`）两个阶段，不包含后续聚类/概念合成
   阶段（那两个阶段是 GraphRAG 独有的图谱构建后处理，与"从一段文本抽取实体
   关系"这一原子能力无关，不适合下放给 Consolidation 复用）。由于
   `graphrag.Entity`/`graphrag.Relation` 本身是 `types.Entity`/
   `types.Relation` 的类型别名（`type Entity = types.Entity`），
   `*GraphBuildPipeline` 结构性满足 `SharedEntityExtractor`，零适配器代码。

3. **三级降级链**（`consolidation_extract.go.extractEntitiesAndRelations`）：
   - `p.entityExtractor != nil` → 唯一主路径，调用共享实现；返回 err 时
     直接降级到 `ruleExtract`（不再退回 `llmExtract`，避免降级路径变成
     第二条 LLM 调用路径，违背本次合并"消除重复燃烧"的初衷）。
   - `p.entityExtractor == nil && p.summarizer != nil` → 历史兼容回退
     `llmExtract`（保留原实现，供未装配 `graphrag` 依赖的部署形态使用，
     如某些不需要 RAG 能力的极简 Tier-0 场景）。
   - 两者皆 nil → `ruleExtract` 正则/共现回退（不变）。

4. **装配位置**：`cmd/polaris/boot_knowledge.go` 在 `graphPipeline`
   （`*graphrag.GraphBuildPipeline`）构造完成后，若 `graphPipeline != nil
   && tb.ConsolidationPipeline != nil`，调用
   `tb.ConsolidationPipeline.WithEntityExtractor(graphPipeline)`。
   `graphPipeline` 由 `FeatureGraphRAGFull` 硬件门控（Tier-0/内存压力下为
   `nil`），此时 `WithEntityExtractor` 不被调用，`ConsolidationPipeline`
   保持注入前的三级降级链最后两级，Tier-0 行为零回归。

## 后果

- 消除了 Episodic 会话文本与 RAG 文档文本各自触发一次独立 LLM 抽取调用的
  重复燃烧；两条业务语境下的实体/关系表述收敛到同一套 Prompt/解析实现，
  降低同一实体在两个来源产生命名/类型漂移的概率。
- `internal/memory` → `internal/knowledge/graphrag` 的依赖通过消费方接口
  （`SharedEntityExtractor`）在 `cmd/polaris` 装配层打断，不违反 R1.4 与
  L1→L2 禁止跨层导入的不变量（`internal/memory` 包本身不 import
  `internal/knowledge/graphrag`）。
- ADR-0074 的写入期去重桥接（`GraphWriter.UpsertEntity` 查重）与检索期
  联合种子（`source_type='graphrag_ingest'`）保持不变、继续生效——本 ADR
  只是让"喂给"这两套下游机制的实体在源头上就更一致，二者是互补而非替代关系。
- 遗留的 `consolidation.llmExtract`/`entity_extraction.tmpl` 未删除，作为
  `graphrag` 依赖未装配场景下的降级路径保留，代价是维护两份 Prompt 模板；
  若未来证实该场景从未被真实使用，可另开 ADR 评估删除。
- 本 ADR 不涉及、不推翻 ADR-0033（记忆子系统范围限制）关于 Tier-0 SQLite /
  Tier-1+ 图存储的分级存储选型结论。
