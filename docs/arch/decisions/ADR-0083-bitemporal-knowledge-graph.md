# ADR-0083: 双时态知识图谱 —— 关系边时态化 + AsOf 视图

- **状态**: Accepted（已执行）
- **日期**: 2026-08-02
- **决策者**: 系统架构师
- **相关模块**: M5 `internal/memory/store/semantic_mem.go`、M10 `internal/knowledge/graphrag/`、`internal/protocol/schema/004_semantic_memory.sql`

## 上下文

`semantic_entities` 已有 `valid_from`/`valid_until`/`status`/`superseded_by`（Zep/Graphiti 双时态模式）。`semantic_relations` 无任何时态列——在"事实即边"的知识图谱模型中，这意味着双时态建模只做了一半：能表达"张三这个实体何时有效"，不能表达"张三任职于 A 公司这一事实何时有效"。检索层（`HybridRetriever`/`GraphTraverser`）也只有实体侧的 Go 层后过滤 AsOf 判断（`resolveSemanticHit`），没有下推到 SQL/索引层，且完全不覆盖关系边。

## 决策

1. **关系边双时态化**：`semantic_relations` 新增 `valid_from`/`valid_until`/`status`/`superseded_by`，语义与实体侧完全对齐（事务时间 = `created_at`/`updated_at`，有效时间 = `valid_from`/`valid_until`）。
2. **唯一性约束改为部分索引**：删除表级 `UNIQUE(source_id, target_id, relation_type)`，改为 `CREATE UNIQUE INDEX uq_semantic_rel_active ... WHERE status='active'`——允许同一三元组保留多个历史版本（`status != 'active'`），只约束"当前活跃版本"唯一。
3. **信念修正策略**：`UpsertRelation` 写入时，若目标三元组已有活跃边且内容发生**实质变化**（`weight` 变化超过 `relation_weight_delta_threshold=0.2`，或 `properties` 内容不同），旧边置 `status='superseded'`/`valid_until=now`/`superseded_by=<新边id>`，插入新的活跃边；否则原地 `UPDATE`（避免版本链无谓膨胀）。
4. **AsOf 视图**：`internal/knowledge/graphrag/temporal_view.go` 新增 `AsOfFilter{At time.Time}`，`SQLWhere(alias)` 返回可拼接 WHERE 片段：零值 → `status='active'`（走 `idx_semantic_rel_status`，与改造前查询计划一致）；非零值 → `(valid_from IS NULL OR valid_from <= t) AND (valid_until IS NULL OR valid_until > t)`。接入 `GraphTraverser.fetchNeighbors` 的 4 条邻居查询与 `findSeedEntities`。
5. **下游读路径按 status 过滤**：`CascadeInvalidator`（级联失效 CTE）与 `cognitive_replayer`（SurrealDB 冷回放）此前遍历 `semantic_relations` 时未过滤 status（此前无该列，等价于全量）；关系边可以有历史版本后，两处必须显式 `WHERE status='active'`，否则历史版本边会污染级联传播与图引擎回放，这是本 ADR 引入新列后**必须同步的下游行为**，不属于范围外顺手修改。
6. **`TemporalExpirer.ExpireStale` 扩展**：在同一次调用内追加对 `semantic_relations` 的到期扫描（`valid_until <= now AND status='active'` → `expired`），不新增 ticker。

## 后果

- **正向**: 可回答"当时以为的事实是什么"（AsOf 回放）；信念修正保留完整历史版本链而非物理覆盖，符合 HE-6 State-in-DB 与审计可追溯要求。
- **负向**: `UpsertRelation` 从单条 UPSERT 变为"读+判定+条件分支写"，多一次读延迟；已知所有依赖旧 `ON CONFLICT(source_id, target_id, relation_type)` 的语句必须同步改为部分索引形式，遗漏会在写入时报 SQL 约束错误而非静默损坏（fail-fast，可接受）。
- **反例守护**: 禁止用 `updated_at` 冒充有效时间；禁止 `AsOf` 实现为"扫全表再 Go 侧过滤"（必须走 `idx_semantic_rel_valid`/`idx_semantic_rel_status`）；禁止引入独立历史版本表（SQLite 上会让图遍历代价翻倍，违反 Tier-0，历史版本就地保留在同一张表内，用 `status` 区分）；禁止关系边的"次要证据累积/权重微调"也走版本升级（会造成版本链爆炸，此类变化原地 UPDATE）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 独立 `semantic_relations_history` 历史表 | 图遍历需要同时查当前+历史时多一次 JOIN/UNION，Tier-0 SQLite 上代价翻倍；同表 status 分区已能表达 |
| 表级 `UNIQUE` 改为 `UNIQUE(source_id, target_id, relation_type, status)` | 仍不足以防止多条 `superseded` 版本互撞（同一三元组可以有 N 条历史版本，`status` 值相同），必须用部分索引只约束 `status='active'` 这一个切片 |
| AsOf 参数解析失败静默退回当前视图 | 会让调用方以为查的是历史，实际拿到当前视图，属静默错误的信息误导；已在 `memory_search` 侧改为 `CodeInvalidInput` 硬失败 |

## 引用代码

- `internal/protocol/schema/004_semantic_memory.sql`（DDL）
- `internal/memory/store/semantic_mem.go`（`UpsertRelation` 信念修正）
- `internal/knowledge/graphrag/temporal_view.go`（`AsOfFilter`）
- `internal/knowledge/graphrag/graph_traverser.go`（4 条邻居查询接入点）
- `internal/memory/retrieval/cascade_invalidator.go` / `cognitive_replayer.go`（status 过滤同步）
- `internal/memory/graph/temporal.go`（`TemporalExpirer` 扩展）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-08-02 | 初稿，随阶段05 P-02 落地 |
