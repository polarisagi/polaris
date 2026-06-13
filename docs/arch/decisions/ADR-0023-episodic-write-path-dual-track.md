# ADR-0023: episodic 写路径双轨制（kv_store 热路径 + OutboxWorker 冷投影）

- **状态**: Accepted
- **日期**: 2026-06-13
- **决策者**: 架构组
- **相关模块**: M05 (Memory), M04 (Agent Kernel), M02 (Storage/Outbox)

## 上下文

M05 架构文档长期将 `episodic_events` 定义为 OutboxWorker 异步投影的派生表，但实际代码中：

1. `EpisodicMem.Append()` 仅写入 `kv_store` KV 表，`episodic_events` 无任何 INSERT 路径
2. `retriever.go:fetchVectorResultsFromSQL` 从 `episodic_events` 读取向量，因表永远为空，Tier-0 向量相似度检索完全失效
3. `online_reindexer.go` 每轮处理 0 行（表空）
4. `EpisodicMem.Append` 将 `json.Marshal(ev)` 整体写入 kv_store，`ev.Payload` 无大小门控，工具输出可达 MB 级导致 SQLite WAL 膨胀

## 决策

**建立 episodic 双轨写路径，并对 kv_store 写入加 8KB Payload 门控。**

**热路径 1（同步，<10ms）**: `EpisodicMem.Append()` → `kv_store`，Payload 超 8KB 截断 + 落盘至 `~/.polarisagi/polaris/logs/events/<id>.bin`，替换为含 512 字节摘要的 `log_ref` JSON 占位符，保留 BM25 可搜索性。

**热路径 2（outbox 触发）**: `agent_execute.go` 在写 kv_store 后同步向 outbox 投递 `target_engine="episodic"` 记录。

**冷投影（OutboxWorker 异步）**: 新增 `EpisodicProjectorHandler`（`pkg/cognition/memory/episodic_projector.go`），消费 outbox 记录后幂等 INSERT `episodic_events` 表（INSERT OR IGNORE），填充 `content`（≤2048 字节）、`salience`、`decay_weight`、`cold` 等字段。同 session 内 seq 距最大 seq 超 1000 的历史行直接标记 `cold=1`。

`OnlineReindexer` 扫描 WHERE 子句增加 `AND cold = 0`，跳过冷事件的 embedding 生成，避免无意义的 8KB BLOB 写入。

## 后果

- **正向**: `episodic_events` 投影写入路径打通，向量相似度检索从"永远返回空"变为可用；kv_store 不再因大型工具输出而膨胀；OnlineReindexer 处理量与 embedding 存储量均受控
- **负向**: 双写引入一次 outbox 写入开销（微秒级，可忽略）；`EpisodicProjectorHandler` 为新增路径，需在 OutboxWorker 初始化时显式注册
- **反例守护**: 禁止将 `EpisodicMem.Append` 改为直接 INSERT `episodic_events`——该表是投影派生表，写入必须经 OutboxWorker 保证幂等性和解耦性

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| `EpisodicMem.Append` 直接双写 kv_store + episodic_events | 破坏 M2 单写者约束（MutationBus）；同步写入两个存储增加热路径延迟 |
| 废弃 kv_store，全量走 episodic_events | BM25 Tier-0 路径依赖 kv_store 前缀扫描；episodic_events 是投影表，不应成为唯一写入目标 |
| 不限制 Payload 大小 | MB 级条目导致 SQLite B+ 树碎片化、WAL 膨胀、BM25 全量反序列化性能退化 |

## 引用代码

- `pkg/cognition/memory/episodic_mem.go`（Append + truncateEpisodicPayload）
- `pkg/cognition/memory/episodic_projector.go`（EpisodicProjectorHandler，新建）
- `pkg/cognition/memory/online_reindexer.go`（cold=0 过滤）
- `pkg/cognition/kernel/agent_execute.go`（outbox 触发写入）
- `internal/protocol/schema/003_episodic_memory.sql`（DDL SSoT，cold 字段）
- `docs/arch/M05-Memory-System.md §3.1 §6`

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-13 | 初稿 |
