# ADR-0003: modernc/sqlite（零 CGO）作为主持久化存储

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M2 / `pkg/substrate/storage`
- **实现详情**: [M02 §1.1](../M02-Storage-Fabric.md) | [00-Dict §6 Storage-SQLite](../00-Global-Dictionary.md)

## 上下文

需要嵌入式持久化:Tier-0 单二进制 + 跨平台零 CGO 交叉编译 + 单用户并发 + SQL 表达力 + FTS5。

## 决策

**采用 `modernc/sqlite`(纯 Go SQLite 端口)作为主持久化存储。**

- WAL + `synchronous=NORMAL` + `_busy_timeout=5000` + `_foreign_keys=ON`
- `MaxOpenConns=1`(单写者,与 MutationBus 串行化契合)
- 所有业务写入必经 `DatabaseWriter`,禁止旁路 INSERT

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| mattn/go-sqlite3 (CGO) | 跨平台交叉编译需 C 工具链;与 purego/Rust FFI 一致性破坏 |
| Postgres / MySQL | 独立服务进程依赖,违反单二进制 + Tier-0 |
| BoltDB / Badger / Pebble | 无 SQL/FTS5/事务粒度,需自建查询层 |
| SurrealDB-Embedded 替代 SQLite | SurrealDB 用于认知检索轴;EventLog/Outbox 仍需强 ACID |

**反例守护**:未来如有人为支持高并发改 Postgres/MySQL—本 ADR 拒绝。polaris 是单用户 Agent,非多用户服务;多用户场景需另起架构。
