# ADR-0010: SurrealDB(Rust FFI 嵌入式)作为认知检索轴

- **状态**: Accepted
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M2 / M5 / M10 / `internal/store/surreal_store.go`
- **实现详情**: [M02 §1.1](../M02-Storage-Fabric.md) | [00-Dict §6 Storage-SurrealDB-Core](../00-Global-Dictionary.md)
- **关联 ADR**: [ADR-0003](./ADR-0003-sqlite-modernc-primary-storage.md)(互补) | [ADR-0005](./ADR-0005-purego-ffi-cedar.md) + [ADR-0011](./ADR-0011-cgo-to-purego-migration.md)(surreal_store.go cgo→purego 迁移已完成 2026-05-16)

## 上下文

cognition/swarm 需多模态检索:KV / 向量近邻(HNSW)/ 图遍历 / 全文检索 BM25。多独立引擎(Qdrant + neo4j + Elasticsearch + Redis)违反单二进制约束。SQLite 单独无法满足(向量索引+图查询不足)。

## 决策

**采用 SurrealDB v3（surrealdb crate，嵌入式）作为认知检索轴，经 purego FFI 桥接。**

**技术选型**:
- 选定库: [SurrealDB](https://github.com/surrealdb/surrealdb)
- Rust crate: `surrealdb = { version = "3", features = ["kv-mem", "kv-rocksdb"] }`
- 嵌入模式: 进程内嵌入（embedded, 无独立服务进程），经 `purego` FFI cdylib 桥接 Go。

SurrealDB 原生支持四轴检索（KV / HNSW 向量 / 有向图遍历 / BM25 全文检索），单一 crate 闭闭环，无多引擎协调开销。

职责分工（与 ADR-0003 互补）：
- **SQLite (modernc/sqlite)**: EventLog / Outbox / 元数据 / FTS5 基础 — 真相源 + 强 ACID（注：BM25 全文检索实际由 SQLite FTS5 承担，位于 `hybrid_retrieve.go`）
- **SurrealDB (Rust FFI 嵌入式)**: KV / HNSW 向量 / 有向图 — 认知检索轴

后端选择策略（运行时配置，`configs/defaults.toml [cognition]`）：
- **surreal-mem（默认）**: `kv-mem` 后端，任意内存机器可用（最低 512MB 总内存）；`vec_max` 启动时按可用内存自动计算；进程重启数据丢失，由 SQLite Outbox 投影恢复（M02 §2.5）
- **surreal-rocksdb（自动或显式）**: `kv-rocksdb` 后端，TotalRAM ≥ 8GB 时自动启用（含 8GB Tier 0 开发机），无需手动配置；RSS 开销 ~200MB，数据持久化落盘 `~/.polarisagi/polaris/data/surreal.db`；<8GB VPS 可显式设置 `backend = "rocksdb"` 强制开启
- 统一经 `StorageRouter` 路由，`Store` 接口屏蔽后端差异

## 后果

- **正向**: 见决策章节
- **负向**: 暂无已知负向后果
- **反例守护**:
  - 未来如有人提议"为 X 引入 Qdrant/neo4j"——本 ADR 拒绝。多引擎依赖与单二进制不兼容
  - 未来如有人提议"用 SQLite 自己做向量近邻"——本 ADR 拒绝。SurrealDB kv-mem 已覆盖任意内存机器
  - 未来如有人提议"直接用 rust-rocksdb 替代 surrealdb"——本 ADR 拒绝。rust-rocksdb 仅提供 KV，缺失 HNSW/图/BM25 三轴
  - 未来如有人提议"恢复自研 BTreeMap 实现"——本 ADR 拒绝。已由 SurrealDB kv-mem 统一替代

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| Qdrant + neo4j + Elasticsearch | 三独立进程；启动成本；跨引擎一致性；违反单二进制约束 |
| 仅 SQLite + 自建向量/图层 | 重复造轮子；HNSW 实现复杂；性能不可控 |
| 仅 BoltDB + 内存索引 | 无 SQL 表达力；图遍历需手撸 |
| 全部 Rust 重写存储层 | 增加 Rust 暴露面；Go 层失去生态（FTS5、迁移） |
| rust-rocksdb 直接使用 | 仅 KV，无向量/图/FTS；需自建三轴索引，等同重造 SurrealDB |
| 自研 BTreeMap + HNSW 实现 | 功能重复 SurrealDB kv-mem；测试覆盖不足；维护两套实现 |

## 引用代码

- `internal/store/surreal_store.go`

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿（回填，初始决策早于 ADR 体系建立） |
| 2026-07-09 | 明确 BM25 全文检索实际由 SQLite FTS5 承担（`hybrid_retrieve.go`），SurrealDB 专注 HNSW 向量 + KV + 图遍历 |
