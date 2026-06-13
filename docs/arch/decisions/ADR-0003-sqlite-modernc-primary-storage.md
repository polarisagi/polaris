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
- `MaxOpenConns=1`（单写者，与 MutationBus 串行化契合）
- **XR-04 三层写路径**（`00-Global-Dictionary §XR-04`，三层均共享同一 `*sql.DB`）：
  1. **高频批量**（events/decision_log）→ `MutationBus DatabaseWriter`（异步批量 + 乐观锁）
  2. **中频同步**（M5/M13/M12）→ `Store.Put` / `Store.Txn`（KV 接口 + 同步确认）
  3. **CAS + 配置管理**（Blackboard 任务状态 / server CRUD）→ `store.DB()` 直写（需同步 RowsAffected，须在 AGENTS.md 或 ADR 显式标注）
- 禁止：同一数据跨层混写（高频数据走裸 db.ExecContext；配置数据走 MutationBus）

> **注（2026-06-13 更新）**：原规则"所有业务写入必经 DatabaseWriter，禁止旁路 INSERT"已被 XR-04 放宽为三层规范。Blackboard CAS 等需要同步确认的操作走第三层直写，不再被视为违规。

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| mattn/go-sqlite3 (CGO) | 跨平台交叉编译需 C 工具链;与 purego/Rust FFI 一致性破坏 |
| Postgres / MySQL | 独立服务进程依赖,违反单二进制 + Tier-0 |
| BoltDB / Badger / Pebble | 无 SQL/FTS5/事务粒度,需自建查询层 |
| SurrealDB-Embedded 替代 SQLite | SurrealDB 用于认知检索轴;EventLog/Outbox 仍需强 ACID |

**反例守护**：未来如有人为支持高并发改 Postgres/MySQL—本 ADR 拒绝。polaris 是单用户 Agent，非多用户服务；多用户场景需另起架构。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-16 | 初稿，Accepted |
| 2026-06-13 | 写路径规则更新为 XR-04 三层规范，废除原"禁止旁路 INSERT"绝对约束 |
