# ADR-0003: 主存储引擎选型合集（modernc/sqlite 主持久化 + SurrealDB 认知检索轴，含原 ADR-0010）

- **状态**: Accepted | **日期**: 2026-05-16 | **模块**: M2/M5/M10 `internal/store`
- **实现详情**: [M02 §1.1](../M02-Storage-Fabric.md) | [00-Dict §6 Storage-SQLite/Storage-SurrealDB-Core](../00-Global-Dictionary.md)

## 决策一：modernc/sqlite（零 CGO）作为主持久化存储（原决策）

采用 `modernc/sqlite`（纯 Go 端口）作为主持久化存储：WAL + `synchronous=NORMAL` + `_busy_timeout=5000` + `_foreign_keys=ON`，`MaxOpenConns=1`（单写者）。

**XR-04 三层写路径**（`00-Global-Dictionary §XR-04`，共享同一 `*sql.DB`）：
1. 高频批量（events/decision_log）→ `MutationBus DatabaseWriter`
2. 中频同步（M5/M13/M12）→ `Store.Put`/`Store.Txn`
3. CAS + 配置管理 → `store.DB()` 直写（须同步 RowsAffected）

禁止同一数据跨层混写。

## 决策二：SurrealDB（Rust FFI 嵌入式）作为认知检索轴（原 ADR-0010，与决策一互补）

采用 SurrealDB v3（`surrealdb` crate，`kv-mem`+`kv-rocksdb`，进程内嵌入）经 purego FFI 桥接（[ADR-0011](./ADR-0011-cgo-to-purego-migration.md)），原生支持 KV/HNSW 向量/图遍历/BM25 四轴检索，避免多引擎（Qdrant+neo4j+ES）协调开销。

职责分工：SQLite 承担 EventLog/Outbox/元数据/FTS5 全文检索（真相源+强 ACID）；SurrealDB 承担 KV/HNSW 向量/图（认知检索轴）。

后端策略（`configs/defaults.toml [cognition]`）：默认 `kv-mem`（任意内存机器可用，重启数据丢失由 SQLite Outbox 投影恢复）；`TotalRAM ≥ 8GB` 自动启用 `kv-rocksdb` 持久化。

## 反例守护

拒绝为支持高并发改用 Postgres/MySQL——polaris 是单用户 Agent，非多用户服务。CGO 版驱动（mattn/go-sqlite3）拒绝，破坏交叉编译一致性。拒绝引入 Qdrant/neo4j 等多引擎依赖——违反单二进制约束。拒绝自建向量近邻/BTreeMap 实现——SurrealDB 已统一覆盖。拒绝裸 rust-rocksdb——仅提供 KV，缺 HNSW/图/BM25。

## 引用代码

`internal/store/surreal_store.go`

## 修订记录

2026-06-13 SQLite 写路径规则更新为 XR-04 三层规范，废除原"禁止旁路 INSERT"绝对约束。
