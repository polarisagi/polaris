# ADR-0003: 主存储引擎选型（modernc/sqlite 主持久化 + SurrealDB 认知检索轴）

> 状态：Accepted ｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0010 ｜ 模块：M2/M5/M10 `internal/store`
> 实现详情：[M02 §1.1](../M02-Storage-Fabric.md)、[00-Dict §6](../00-Global-Dictionary.md)

## 决策一：modernc/sqlite（零 CGO）为主持久化

WAL + `synchronous=NORMAL` + `_busy_timeout=5000` + `_foreign_keys=ON`，`MaxOpenConns=1`（单写者）。

XR-04 三层写路径（`00-Global-Dictionary §XR-04`，共享同一 `*sql.DB`）：
- 高频批量（events/decision_log）→ `MutationBus DatabaseWriter`
- 中频同步（M5/M13/M12）→ `Store.Put`/`Store.Txn`
- CAS + 配置管理 → `store.DB()` 直写（须同步 RowsAffected）

禁止同一数据跨层混写。

## 决策二：SurrealDB（Rust FFI 嵌入式）为认知检索轴（原 ADR-0010）

SurrealDB v3（`kv-mem`+`kv-rocksdb`，进程内）经 purego 桥接（[ADR-0011](./ADR-0011-cgo-to-purego-migration.md)），统一 KV / HNSW 向量 / 图遍历 / BM25，避免多引擎协调开销。
- 分工：SQLite 管 EventLog/Outbox/元数据/FTS5（真相源，强 ACID）；SurrealDB 管 KV/HNSW/图。
- 后端（`configs/defaults.toml [cognition]`）：默认 `kv-mem`，重启丢失由 SQLite Outbox 投影恢复；`TotalRAM ≥ 8GB` 自动启用 `kv-rocksdb`。

## 反例守护 / 已驳方案

- Postgres/MySQL — 单用户 Agent 非多用户服务
- mattn/go-sqlite3（CGO） — 破坏交叉编译一致性
- Qdrant/neo4j 等多引擎 — 违反单二进制约束
- 自建向量近邻/BTreeMap — SurrealDB 已覆盖
- 裸 rust-rocksdb — 仅 KV，缺 HNSW/图/BM25

## 重新评估触发条件

- 出现真实多用户/多进程并发写需求 → 重议 Postgres/MySQL
- kv-mem 在核心路径内存预算下实测持续超预算且 kv-rocksdb 无法兜底 → 重议拆分认知检索轴

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-06-13 | 写路径改为 XR-04 三层规范，废除"禁止旁路 INSERT"绝对约束 |
| 2026-07-28 | 并入原 ADR-0010 |

## 引用代码

`internal/store/surreal_store.go`
