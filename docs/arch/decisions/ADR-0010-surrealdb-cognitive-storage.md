# ADR-0010: SurrealDB（Rust FFI 嵌入式）作为认知检索轴

- **状态**: Accepted | **日期**: 2026-05-16 | **模块**: M2/M5/M10 `internal/store/surreal_store.go`
- **实现详情**: [M02 §1.1](../M02-Storage-Fabric.md) | [00-Dict §6 Storage-SurrealDB-Core](../00-Global-Dictionary.md)
- **关联**: [ADR-0003](./ADR-0003-sqlite-modernc-primary-storage.md)（互补）| [ADR-0011](./ADR-0011-cgo-to-purego-migration.md)（FFI 迁移）

## 决策

采用 SurrealDB v3（`surrealdb` crate，`kv-mem`+`kv-rocksdb`，进程内嵌入）经 purego FFI 桥接，原生支持 KV/HNSW 向量/图遍历/BM25 四轴检索，避免多引擎（Qdrant+neo4j+ES）协调开销。

职责分工：SQLite 承担 EventLog/Outbox/元数据/FTS5 全文检索（真相源+强 ACID）；SurrealDB 承担 KV/HNSW 向量/图（认知检索轴）。

后端策略（`configs/defaults.toml [cognition]`）：默认 `kv-mem`（任意内存机器可用，重启数据丢失由 SQLite Outbox 投影恢复）；`TotalRAM ≥ 8GB` 自动启用 `kv-rocksdb` 持久化。

## 反例守护

拒绝引入 Qdrant/neo4j 等多引擎依赖——违反单二进制约束。拒绝自建向量近邻/BTreeMap 实现——SurrealDB 已统一覆盖。拒绝裸 rust-rocksdb——仅提供 KV，缺 HNSW/图/BM25。

## 引用代码

`internal/store/surreal_store.go`
