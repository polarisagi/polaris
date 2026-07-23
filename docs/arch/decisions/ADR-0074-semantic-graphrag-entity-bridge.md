# ADR-0074: Semantic 与 GraphRAG 最小整合桥接

## 状态
Accepted（已执行）

## 背景
原始设计 GD-13-002 建议将 Semantic (M5) 完全并入 GraphRAG (M10)，使 M5 只保留 Working/Episodic/Procedural，但这涉及改动面过大，且违背 Tier-0 使用 SQLite、Tier-1+ 使用图存储的分级架构设定。
由于 M5 和 M10 两个管线相互独立，容易导致同样的实体在两边产生重复数据，并且相互检索不到。

## 决策
采用最小整合桥接方案，在保留双轨架构的前提下解决重复问题：

1. **写入期实体去重桥接**：
   - `GraphWriter.UpsertEntity` 在插入前先查询 `semantic_entities` 表。若同名实体已存在，则走信念修正，而不产生重复。
   - `Consolidation` 管线抽取 Semantic 实体时也会调用 GraphRAG 的实体检索 `graphFetcher` 进行去重。
   - 增加 `source_type`='graphrag_ingest' 来标记来源于 GraphRAG 管线的实体。

2. **检索期联合种子**：
   - 修改 `SemanticMem` 检索，将 `source_type='graphrag_ingest'` 也包含在 Spreading Activation 检索的种子里，使其能联合查询外部文档与 Agent 的知识。

3. **范围声明**：
   - 本次只做实体去重桥接与检索种子统一，不做 M5→M10 物理合并；两套管线的写入 API、生命周期管理、Tier0/Tier1+ 存储选型均保持独立。
   - 完整合并留待未来专项评估，不在本 ADR 范围内。

## 后果
- 解决了知识双写的重复问题。
- 检索能够发现跨管线的数据。
- 是对 ADR-0033 记忆子系统范围限制的补充，而不改变其既有分级选型策略。
