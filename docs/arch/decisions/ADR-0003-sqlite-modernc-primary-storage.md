# ADR-0003: modernc/sqlite（零 CGO）作为主持久化存储

- **状态**: Accepted | **日期**: 2026-05-16 | **模块**: M2 `internal/store`
- **实现详情**: [M02 §1.1](../M02-Storage-Fabric.md) | [00-Dict §6 Storage-SQLite](../00-Global-Dictionary.md)

## 决策

采用 `modernc/sqlite`（纯 Go 端口）作为主持久化存储：WAL + `synchronous=NORMAL` + `_busy_timeout=5000` + `_foreign_keys=ON`，`MaxOpenConns=1`（单写者）。

**XR-04 三层写路径**（`00-Global-Dictionary §XR-04`，共享同一 `*sql.DB`）：
1. 高频批量（events/decision_log）→ `MutationBus DatabaseWriter`
2. 中频同步（M5/M13/M12）→ `Store.Put`/`Store.Txn`
3. CAS + 配置管理 → `store.DB()` 直写（须同步 RowsAffected）

禁止同一数据跨层混写。

## 反例守护

拒绝为支持高并发改用 Postgres/MySQL——polaris 是单用户 Agent，非多用户服务。CGO 版驱动（mattn/go-sqlite3）拒绝，破坏交叉编译一致性。

## 修订记录

2026-06-13 写路径规则更新为 XR-04 三层规范，废除原"禁止旁路 INSERT"绝对约束。
