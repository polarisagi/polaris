
# 模块 2: Storage Fabric

> 多存储引擎并存，全部可嵌入。Go 编排/接口/Outbox Worker/Schema Migration，Rust 侧车热路径引擎 FFI。
> [HE-Rule-3] [HE-Rule-5] [HE-Rule-6] [Tier-0-Limit] [Day0-ColdStart] [Phase0-Bootstrapping]
<!-- §跳读: 0-bis:7 职责 / 0-ter:18 不变量速查 / 1:31 接口层 / 2:57 EventLog / 2.6:136 tasks表 / 3:171 容量 / 4:218 Workspace / 5:248 SchemaManager / 6:260 Reindexer / 7:273 Go↔Rust FFI / 8:285 连接池 / 9:298 374 多写者 / 10:311 387 引擎速查 / 11:324 402 四层记忆映射 / 15:332 410 (SOFT)降级 / 16:346 424 依赖 -->
## 0-bis. 职责边界

- M2 **是**: 多引擎统一抽象接口（Store interface） | **不是**: 具体引擎内部实现
- M2 **是**: EventLog 真相源存储（events 表 append-only） | **不是**: 事件语义解释者（各业务模块）
- M2 **是**: 跨引擎 Outbox 异步投影 + MutationBus 单写者 | **不是**: 跨引擎 ACID 保证（嵌入式不可实现）
- M2 **是**: SQL Schema Migration 管理 | **不是**: 业务缓存策略（M5 自行管理）
- M2 **是**: 多引擎数据路由（Storage-Router） | **不是**: 索引构建逻辑（M10/KB 自行管理）
- M2 **是**: DDL 物理 schema 权威定义 | **不是**: 跨模块接口契约定义（`internal/protocol/interfaces_store.go`）

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M2_01 | events 表仅 INSERT，禁止 UPDATE/DELETE | DDL 权限 + CI（Continuous Integration，持续集成） `eventlog_append_only` 测试 |
| inv_M2_02 | 所有状态变异经 MutationBus DatabaseWriter 单写者串行化 | CI `mutation_bus_lint` 静态扫描 |
| inv_M2_03 | 跨引擎不承诺 ACID——走 outbox + 幂等键 + 最终一致 | 集成测试验证幂等重放 |
| inv_M2_04 | embed_model_version 是一等字段——每向量携带，跨版本检索走 OnlineReindexer backfill | DDL CHECK 约束 |
| inv_M2_05 | 死信记录绝不静默丢弃——超 max_attempts 进 dead 状态 + 告警 + 人工介入 | outbox.dead.count >0 告警 |
| inv_M2_06 | audit_log 表 DDL 触发器禁止 UPDATE/DELETE（append-only 硬约束） | CI `audit_integrity` 测试 |

---

## 1. 统一接口层

### 1.1 Store 接口

契约权威源 `internal/protocol/interfaces_store.go` 的 `Store` / `Transaction` 接口，所有引擎适配器须实现。

### 1.2 [Storage-Router]

实现 `internal/store/`（StorageRouter）。按优先级匹配路由规则，全未命中回落 SQLite；SurrealDB FFI 加载失败时路由表置空，所有请求直连 SQLite。

### 1.3 路由规则表

| 数据类型 | 访问模式 | 延迟要求 | 存储引擎 |
|---------|---------|---------|---------|
| Agent 会话状态/配置 | 随机读写 | <1ms | [Storage-SQLite] |
| Embedding 向量 | 批量写 + KNN 读 | <5ms | [Storage-SurrealDB-Core] |
| 事件日志 | Append-only 写 + 时序扫描 | <100us 写 | [Storage-SQLite] WAL |
| 技能缓存（热点） | 高频读 | <10us | [Storage-SurrealDB-Core] |
| 知识图谱遍历 | 随机多跳读 | <10ms | [Storage-SurrealDB-Core]（Rust FFI via purego） |
| 知识片段 / 全文检索 | 批量写 + ad-hoc 查询 | <50ms | [Storage-SurrealDB-Core] |
| 路由表/元数据 | 高频读 + 低频写 | <1us | sync.Map / [Storage-SurrealDB-Core] |

> 三级 RAM 降级见 §10。

---

## 2. EventLog — 真相源

[HE-Rule-6] 所有状态持久化落盘。EventLog 是唯一真相源，派生引擎状态均可从 events + outbox 重建。

### 2.1 events 表（EventLog 物理存储）

DDL 见 `internal/protocol/schema/001_events.sql`。

- **序列化**: Protobuf（Schema 演化，Go↔Rust 同 .proto 生成，体积缩减 ~60-80%）
- **ID**: ULID（时间有序）；Offset: AUTOINCREMENT（免疫 NTP 漂移/时钟回退）
- M5 `episodic_events` 为此表派生投影表，通过 `idempotency_key` 关联
- **冷热分离**：EventArchiver 随 ConsolidationWorker 定期把超过 `EventlogWarmDays`（默认 30 天）的事件迁入 `data/cold/events_archive.db`（SQLite `ATTACH DATABASE`，零 CGO，见 §3）

**接口**: `Read(ctx, fromOffset, maxBatch) → (events, error)` | `Subscribe(ctx, fromOffset) → (chan Event, error)`

### 2.2 EventLog 写入 — SQLiteEventLog 直接提交

实现 `internal/store/audit/eventlog.go` 的 `SQLiteEventLog`（`cmd/polaris/boot_substrate.go` 装配为 `protocol.EventLogger`）。Agent Emit → `SQLiteEventLog.AppendEvent` → 逐事件同步 `Submit` 到 MutationBus → DatabaseWriter 串行 INSERT；调用方阻塞到 `resultCh` 返回（失败原样返回，无静默丢弃），无批量缓冲、无租约二次校验，故无"SIGKILL 丢失窗口"。

已删除：原设计 EventWriteBuffer（MPSC 批量缓冲）零接线，2026-07-08 删除。

### 2.3 MutationBus + DatabaseWriter — 通用状态变异串行化

实现 `internal/store/`：`mutation_bus.go`（接口+投递）、`mutation_bus_execute.go`（执行循环）、`internal/store/search/embedding_batcher.go`（向量批处理）、`internal/store/search/reranker.go`（重排序）；事件写入见 §2.2。

禁止业务 goroutine 直接 BEGIN IMMEDIATE 写 SQLite。所有变异经 MutationBus 投递意图（表名、操作类型、优先级、组合 ID），DatabaseWriter 单 goroutine 串行执行。

- 普通投递：channel + 指数退避重试（10ms→2s），严禁同步执行兜底
- ETL 批量投递：微事务分片（MaxRowsPerTx=50），分片间让出调度
- 组合投递：同 GroupID 成员单事务原子提交
- 执行循环：batch + ticker 消费，崩溃后自动重启
- 阈值见 `spec/state.yaml §m2_storage`

必须走 MutationBus 的路径（CI `mutation_bus_lint` 强制）: M5 NotesStore.Set/Delete；M2 OutboxWorker 状态更新；sys_config/schema_versions（仅 SchemaManager 维护模式豁免）。

### 2.4 优雅关闭

SIGTERM/SIGINT → 取消全局 context → 等 MutationBus/DatabaseWriter 处理完已提交 intent → os.Exit(0)。

### 2.5 Outbox 模式 — 跨引擎最终一致

以 EventLog 为真相源 + Outbox 投影 + 幂等键。DDL 见 `internal/protocol/schema/002_outbox.sql`。

**CompositeMutationIntent 执行边界**: events 统一走 DatabaseWriter 单写者；路径 `AppendEvent` → `MutationIntent{Table="events", Operation="insert_event"}` → 串行 INSERT（§2.2）。event 写入与业务表写入同事务原子提交，CompositeMutationIntent 天然跨 events + 业务表。这是 [HE-Rule-6] 唯一路径（崩溃恢复时 EventLog 与业务状态因果一致）。

OutboxWorker（`internal/store/outbox_worker.go`）：
- **拉取**：主查询取 cursor 之后的 pending/failed；补充查询捕获 cursor 之前遗漏的失败记录（最多 batchSize/4）
- **Handler**：`RegisterHandler(taskType, handler, checker...)` 注册目标引擎处理器（如 `m10_graph_build`/`episodic`），可选版本高水位检查器；按 `target_engine` 路由
- **Cursor**：持久化到 `sys_config`（key=`outbox_cursor`），`loadCursorSafe` 启动恢复，`saveCursor` 每批提交后原子 CAS。**L3 fail-safe**：`sql.ErrNoRows`（首次启动）合法，从 0 开始；其余 Scan 失败不得退回零值（会从头重放全部 outbox），返回 `ok=false`，`Run()` 本轮跳过、下一 tick 重试（`metrics.RecordOutboxCursorError(ctx,"load"|"save")`）
- **指数退避**：backoff = min(outbox_backoff_initial_ms << attempts, outbox_backoff_max_ms)；值见 `spec/state.yaml §thresholds/m2_storage`（Go 侧 `config.M2StorageThresholds.OutboxBackoffInitialMs` / `OutboxBackoffMaxMs` / `OutboxMaxAttempts`），启动点 `boot_substrate.go` 经 `NewOutboxWorker` 注入，禁止内联字面量（DR-1-007）。`pollInterval`（5s）无阈值键，保留字面量
- `next_retry_at`（业务设置的最早执行时间下界）与退避共同生效，取较大值

Idempotency Key 格式: `{target_engine}:{entity_type}:{entity_id}:{operation}:{version}`

版本高水位拦截: 目标引擎写入前校验 existing_version >= incoming_version → 丢弃并返回 ErrVersionStale（单消息幂等升级为版本偏序幂等）。

**Outbox 表关键列**（DDL 权威见 `002_outbox.sql`）:

| 列 | 类型 | 语义 |
|---|------|------|
| `id` | INTEGER PK | AUTOINCREMENT，全局单调游标 |
| `target_engine` | TEXT | 目标 handler，如 `m4_provider_recovery`/`m10_graph_build` |
| `payload` | TEXT | JSON/msgpack 业务负载 |
| `idempotency_key` | TEXT UNIQUE | 防重复投递 |
| `status` | TEXT | pending/processing/done/failed(指数退避待重试)/dead(毒丸) |
| `attempts` | INTEGER DEFAULT 0 | 已尝试次数，`>= max_attempts` 置 dead |
| `crash_recovery_count` | INTEGER DEFAULT 0 | Poison Pill 计数，`>= 3` 直接置 dead |
| `next_retry_at` | TEXT (nullable) | 业务级最早可处理时间（UTC ISO 8601），独立于退避；fetchBatch 主查询与迟提交补偿均检查 `next_retry_at IS NULL OR next_retry_at <= now`；实际是否拉取取 `max(business_next_retry_at, backoff_time)`（退避时间不持久化为列） |
| `created_at` / `updated_at` | TEXT | 投递 / 最后状态变更时间 UTC |

- **Poison Pill**：Worker 执行 FFI 前原子 `UPDATE outbox SET status='processing', crash_recovery_count = crash_recovery_count + 1 WHERE id = ?`；`>= 3` → 直接 dead，阻断确定性崩溃循环
- **卡死 processing 恢复**：Worker 启动时 `processing → pending`；Janitor 每 5 分钟恢复 `processing AND updated_at < now() - 300s`
- **清理**：`status IN ('done','dead') AND created_at < now() - 7d`，Janitor 每 6h 批量 DELETE（<=1000 行/批 + Gosched）
- **监控**: outbox.pending.count | outbox.lag.seconds | outbox.dead.count (>0 告警)
- **资源压力推迟（2026-09-25）**: Worker 以 `protocol.WithDeferrableBackgroundWork` 运行，handler 内 LLM 调用按后台优先级准入；被内存/CPU 水位线拒绝时路由立即返回 `protocol.ErrBackgroundDeferred`（不挂起）。handler 须沿错误链上抛该哨兵；Worker 置 `failed` + `next_retry_at = now+30s`，**不累加** `attempts` / `crash_recovery_count`——推迟不是失败，不得把投影推进死信（inv_M2_05）。指标 `polaris.outbox.deferred_total{engine}`
- **Embedding 维度**：由 M1 `Embedder.Dimension()` 运行时返回，禁止编译期硬编码；变更触发 OnlineReindexer；不匹配返回 ErrDimensionMismatch，调用方降级 BM25/FTS5

---

## 2.6 tasks 表 — Agent 任务状态核心列

DDL 权威 `internal/protocol/schema/007_tasks.sql`；下表为最终列集合。

| 列 | 类型 | 语义 |
|---|------|------|
| `task_id` | TEXT PK | ULID |
| `session_id` | TEXT | 所属 Session，关联 events |
| `status` | TEXT | Pending/Claimed/Executing/Done/Failed/Suspended/Compensating |
| `priority` | INTEGER DEFAULT 1 | 0=用户交互 / 1=前台辅助 / 2=后台优化 / 3=最低（Auto-Curriculum） |
| `intent` | BLOB (nullable) | TaskEntry.Intent 持久化落点，Worker 认领后读取 |
| `claimed_by` / `claimed_at` | TEXT (nullable) | 认领 agentID / 认领时间 UTC；nil=未认领 |
| `expires_at` | TEXT (nullable) | 租约到期 UTC；Reaper 据此驱逐 |
| `version` | INTEGER DEFAULT 0 | 乐观锁；CAS Claim/BeginExecution/Reaper 均递增 |
| `replan_count` | INTEGER DEFAULT 0 | ReplanGuard 计数；>= MaxReplanAttempts (`spec/state.yaml §m4_kernel.max_replan_attempts`) → S_FAILED |
| `retry_count` / `max_retries` | INTEGER DEFAULT 0 / 3 | 当前重试数 / 上限 |
| `depends_on` | TEXT (nullable) | JSON array of task_id，Macro-DAG 前驱依赖 |
| `result` | BLOB (nullable) | CompleteTask 写入的产出，供委派/编排子任务结果回读（transfer_to_agent 恢复分支、PatternDebate 辩论历史） |
| `error` | TEXT (nullable) | 失败信息 |
| `suspend_reason` | TEXT (nullable) | 枚举 `hitl` / `provider_exhausted` / `killswitch` |
| `provider_suspended_count` | INTEGER DEFAULT 0 | provider_exhausted 自动唤醒计数；> 5 触发 [ESCALATE] + HITL，转 HITL-Suspended TTL 管理 |
| `intent_taint` / `result_taint` | INTEGER DEFAULT 0 | TaintLevel（0=None~4=UserReviewed），随 Intent/Result 跨 Agent 边界传递（inv_M8_05），只升不降 |
| `pipeline_id` / `pipeline_stage` | TEXT (nullable) | 流水线实例 ID / 阶段名（research/plan/execute/verify），见 M08 §5 |
| `context_payload` | TEXT (nullable) | 前序阶段结构化产出（JSON），S_PERCEIVE 优先读取 |
| `namespace` | TEXT (nullable) | 协同任务共享记忆命名空间（GD-14-001），同 namespace 的 Worker 可互相检索 episodic 片段 |
| `spawn_depth` | INTEGER DEFAULT 0 | `TaskEntry.SpawnDepth` 落点，`transfer_to_agent` 委派深度限制（ADR-0111，原 0084） |
| `trace_id` / `span_id` | TEXT (nullable) | 关联 OTel Span |
| `tokens_input` / `tokens_output` / `tokens_cache_read` | INTEGER DEFAULT 0 | 任务累计 LLM token（HE-Rule-1） |
| `cost_usd` | REAL DEFAULT 0.0 | 任务估算费用 |
| `created_at` / `updated_at` | TEXT | UTC |

注：PII 快照存独立表 `task_pii_vault`（`046_task_pii_vault.sql`，ADR-0104 决策八）；`suspend_reason` / `provider_suspended_count` 解决 SessionPIIVault 跨 Provider 熔断的状态持久化，见 M4 §8、M11 §5.1。

---

## 3. EventLog 容量预算与冷热归档策略

### 3.1 Hot/Warm/Cold 三级存储

| 层级 | 保留期 | 存储 | 查询 | 触发条件 |
|------|--------|------|------|---------|
| Hot | 当前 Session | SQLite events 表 | 全字段索引 | 写入即时 |
| Warm | `eventlog.warm_days`（默认 30 天，`internal/config/thresholds.go` M2StorageThresholds） | SQLite events 表 | 全字段索引（低优先级） | 越过 warm_days 且磁盘水位低于阈值 → 物理迁移 |
| Cold | 永久 | 二级 SQLite（`data/cold/events_archive.db`） | `ATTACH DATABASE` 后标准 SQL | 越过 warm_days **且**空闲磁盘占比 < `eventlog.disk_watermark_pct`（默认 20%） |

> Cold 层曾设计为 Parquet + DuckDB，与零 CGO 约束（ADR-0011）冲突而放弃。需对外可读时可叠加"导出 Parquet 快照"离线工具，但不得作在线回读引擎。

### 3.2 磁盘水位触发归档

`internal/memory/consolidation.EventArchiver` 在 `ConsolidationWorker`（6h ticker，`cmd/polaris/boot_tools.go`）每次触发时先探测 `data/cold/` 所在文件系统空闲占比：

- ≥ `eventlog.disk_watermark_pct` → 跳过本轮，不执行任何 `ATTACH`/`INSERT`/`DELETE`（避免 Tier-0 写放大）
- < 阈值 → `ATTACH` → 迁移 `created_at < now - warm_days` 的行 → 主库 `DELETE` → `DETACH`，单事务原子
- 探针失败 → fail-open，退回无条件执行（避免归档永久停摆）
- `disk_watermark_pct <= 0` → 门控禁用，等价无条件周期归档

D2 (性能) 触发: Hot 表行数 >100 万或空间 >500MB → 自动触发 Warm 压缩（`thresholds.go` 的 `EventlogHotRowLimit` / `EventlogHotSizeMB`，`event_archiver.go` 接入）。

### 3.3 容量预算归档表

| 表 | HT0 稳态 (MB) | HT0 峰值 (MB) | 备注 |
|----|-------------|-------------|------|
| events（EventLog） | ~100 | ~200 | ~50-100 事件/Session, ~1KB/行 |
| outbox | ~10 | ~30 | 临时投影队列 |
| episodic_events（向量投影） | ~80 | ~150 | ~768d, 压缩率 ~5x |
| decision_log | ~20 | ~50 | ~10 条决策/Session |
| tasks | ~10 | ~20 | Agent 任务状态 |
| workspace 文件系统 | ~50 | ~200 | 爬取结果等中间物 |
| 索引 + 临时 | ~40 | ~100 | 向量索引 + FTS5 + 迁移备份 |
| **EventLog 合计** | **~310** | **~750** | M2 总预算 310MB (HT0 steady) 的主体 |

### 3.4 归档实现接口

`internal/memory/consolidation/event_archiver.go` 的 `EventArchiver`：
- 构造 `NewEventArchiver(db, warmDays, coldDBDir, diskWatermarkPct)`，`cmd/polaris/boot_tools.go` 以 `sb.Cfg.Thresholds.M2Storage.{EventlogWarmDays, EventlogDiskWatermarkPct}` 注入，周期调用 `Archive(ctx)`
- 水位探测 `diskFreeRatio(path)`（`internal/memory/consolidation/disk_probe_unix.go` / `disk_probe_windows.go`）
- 动作：`ATTACH ... AS cold_archive` → 建表（与 `001_events.sql` 一致）→ 单事务 `INSERT ... SELECT` + `DELETE` → `COMMIT` → `DETACH`；无新增 CGO 依赖

**与 M5 §10.2 的区别**：M5 的"冷归档"（`ColdArchiver`，`internal/memory/consolidation/consolidation_archive.go`）是记忆遗忘机制（archive-prefix 键 + tombstone + VACUUM）；`EventArchiver` 是 EventLog 层物理迁移，职责与代码路径独立，不互相替代。

---

## 4. WorkspaceManager — 重型中间物文件系统

大规模爬取结果、AST dump、diff patch、二进制文件不入 SQLite，不入 Working Memory（[Tier-0-Limit]，仅持路径+摘要）。物理路径 `~/.polaris/workspace/<task_id>/`，权限 0700。

实现 `internal/store/`（WorkspaceManager）：
- 创建任务隔离目录并幂等注册 manifest；启动时自动建根目录
- 文件注册写 manifest，供配额累计
- 写入前配额校验：累积占用 + 待写大小 > maxSize（Tier 0 = 500MB）拒绝；workspace_write 前 CheckQuota，超限 ErrQuotaExhausted
- GC：物理删除超 7 天的非活跃工作区，跳过活跃任务
- DirPath：返回物理路径（不创建）
- 每 30s OTel 探针上报可用空间，<100MB → CRITICAL + 暂停所有 workspace_write

**Workspace GC**：M13 ResourceReaper 每日 04:00 回收 >7 天且关联 Task 为 Done/Failed 的 workspace；写入拦截触发时 Reaper.RunNow()。

**绝对生命周期上限（防永久泄漏）**:
- **HITL-Suspended 超时**（`suspend_reason='hitl'`，默认 TTL=30 天可配）:
  - 提前 5 天: ResourceReaper 写 `hitl_suspension_expiry_warning` WARN 审计 + 操作员通知
  - 到期: (a) 清零 task_pii_vault 行（SecureZero；PII 先于一切删除）→ (b) MutationBus 置 S_FAILED + 写 `suspended_hitl_timeout_expired` → (c) HITL 通知（M13）→ (d) 之后 7 天走正常 GC
- **KillSwitch-Suspended**: 无 TTL（等 unseal 自动恢复）；但磁盘 <100MB CRITICAL 且 workspace UpdatedAt >7 天 → 打包 `~/.polaris/archive/<task_id>_<timestamp>.tar.zst` 删原目录，保留 Blackboard 元数据；unseal 时 M13 先解压再恢复；归档上限 10GB（LRU 删最老 + WARN）
- **Dead-letter Pending**: `status=Pending` 且 Outbox `status='dead'` 且 UpdatedAt+7d>now → 直接 S_FAILED + GC workspace
- **provider_exhausted-Suspended**: 无 TTL；M1 CircuitBreaker 恢复（M1 §7.3）触发自动唤醒；`provider_suspended_count > 5` 已 ESCALATE→HITL，转 HITL-Suspended TTL

> 可调参数: `hitl_suspension_ttl_days` = 30（`polaris config set ... N`，最小 7）；`archive_max_size_gb` = 10。

**静态加密**: 外部 Connector (M10) 拉取的原始文件落盘前 AES-256-GCM 加密（key 由 M11 CredentialVault.persistent_key 派生，与 M11 SafeString HMAC 共享）。强制: `[TaintLevel]` ≥ `[Taint-Medium]`；可跳过: `[Taint-Low]`/`[Taint-None]`。

**VFS 引用计数**: 热表大型载荷（>4KB）不入 B-Tree 页，写 VFS 文件 `~/.polaris/vfs/{sha256_prefix}/{uuid}.blob`，热表仅存 `vfs_ref` 指针（4KB 热行硬限防页缓存血崩，CI `migration_lint` 强制）。引用表 `sys_vfs_references(vfs_ref TEXT PK, ref_count INTEGER)`，BEFORE DELETE trigger 递减，归零入队 GC。

---

## 5. Schema 迁移策略

**当前阶段（上线前）**：直接修改 `internal/protocol/schema/NNN_*.sql` 原始 DDL，删库重建（`rm ~/.polaris/data/polaris.db`）。禁止 ALTER TABLE/ADD COLUMN 补丁文件。

**上线后**：新增编号迁移文件，由 `internal/store/`（SchemaManager）按版本升序执行，每条独立事务（失败回滚），前后向 `sys_config` 写状态标记（idle / in_progress / completed）；启动检测到 `in_progress` 则拒绝启动，要求操作员重置。SchemaManager 骨架已接入 `store.OpenSQLite` 启动流程。

**兼容策略**：新字段一律 NULLABLE 或有 DEFAULT；降级不执行 Down，旧代码忽略新列。

冷存储 EventLog 重放: 优先 M4 FSM Snapshot；不可用则 `ATTACH DATABASE data/cold/events_archive.db` 标准 SQL 回读。

---

## 6. OnlineReindexer — 零停机向量索引重建

实现位置 `internal/memory/`（M5 记忆层，非 substrate）。对抗 embedding 空间漂移（3-6 月周期）：基于 `embed_model_version` 检测未索引条目，批量后台回填（batchSize=50，best-effort）。

- **回填**：后台构建新版本索引，throttle ≤100 rows/s，Outbox 双写保证增量一致
- **切换**：新旧索引版本指针原子更新，旧索引保留 7 天供回退
- **降级**：新索引 Recall@5 ≤ 旧索引 90% → ABORT，保留旧索引，WARN
- **崩溃恢复**：启动扫描 `reindex_progress`，按状态（backfilling/swapping）恢复或回滚

监控: polaris_reindex_progress (0.0-1.0 gauge) | polaris_reindex_rows_per_second | polaris_surreal_index_versions

---

## 7. Go↔Rust FFI 集成边界

> 编码级约定（purego ABI、文件组织、Cargo.toml）见 `docs/specs/02-Rust-FFI.md`。

- Rust 侧：跨 FFI 函数必须 catch_unwind（panic 不可跨 FFI）；返回 i32 错误码（0=成功），详情走 thread-local last_error()；cbindgen 生成 C 头文件，CI 提交
- 内存所有权：Go→Rust 短生命周期入参由 Go 分配/释放；Rust→Go 大批量返回值由 Go 预分配 buffer、Rust 写入
- 编译：CI 矩阵预编译三平台 Rust 静态库，Go build 不触发 Rust 编译
- 日志：Rust tracing → FFI 桥接 Go slog，不走 stdout/stderr
- 崩溃分层防御: L1 CI ASAN/Valgrind；L2 `debug.SetPanicOnFault(true)` 将 SIGSEGV 转 Go panic（suture 可捕获）；L3 systemd/launchd Restart=always + EventLog 回放恢复

---

## 8. SQLite 连接池

读写双连接池分离，同一 DB 文件对应两个 `*sql.DB`，实现 `internal/store/store.go`（`SQLiteStore.OpenSQLite`）：

- **writer**：`MaxOpenConns=1`，与 MutationBus 单写者对齐；供 `SQLiteStore.ExecContext`、`store.DB()`（Blackboard CAS / 复杂 SQL / DatabaseWriter）、Outbox Worker 使用
- **reader**：`MaxOpenConns=4`，DSN 加 `PRAGMA query_only=1` 引擎层禁写；供 `QueryContext`/`QueryRowContext`、`store.ReadDB()`（网关只读管理接口、`MemoryAgent.ScanHighSalience` 等后台扫描）使用

WAL 下多 reader 与单 writer 互不阻塞；读写共用一个 `*sql.DB` 会被连接池串行化，长批量写独占连接时只读接口无限挂起，双池分离即修复。

所有模块必须经 `protocol.Store`/`protocol.SQLQuerier` 访问 SQLite，禁止自行 `sql.Open`——`internal/store/store.go` 是全仓库唯一生产级调用点。

---

## 9. 多写者防御层级 (WAL 模式)

| 层 | 机制 | 阈值 |
|---|------|------|
| L0 | MutationBus channel 缓冲 + DatabaseWriter 批量执行（§2.3） | `spec/state.yaml §m2_storage.mutation_bus_channel_cap` / `max_batch_size` / `ticker_interval_ms` |
| L1 | PRAGMA busy_timeout | `spec/state.yaml §m2_storage.sqlite_busy_timeout_ms` |
| L2 | PRAGMA wal_autocheckpoint | `spec/state.yaml §m2_storage.wal_checkpoint_pages` |
| L3 | WAL 大小监控 + PASSIVE/RESTART checkpoint | WAL>50MB PASSIVE, >200MB RESTART, 30s 检查 |
| L3.5 | WAL 临界截断 (sqlite3_interrupt + TRUNCATE) | WAL>500MB, 1s 检查 |
| L4 | 读事务 ctx 超时 + 分页释放读锁 | ctx.WithTimeout(5s) |

---

## 10. 引擎选择速查（2026 极简双轴架构）

- **[Storage-SQLite]**（modernc.org/sqlite，纯 Go CGO-Free）：系统控制轴。唯一真相源 (EventLog)、任务状态机、ACID 队列 (Outbox)。
- **[Storage-SurrealDB-Core]**（`surrealdb` crate，Rust cdylib via purego）：认知检索轴。嵌入式原生提供 KV + HNSW 向量 + 图遍历 + BM25，单 crate 四轴闭环。决策与被驳方案见 [ADR-0003](./decisions/ADR-0003-sqlite-modernc-primary-storage.md)（决策二，含原 ADR-0010）。
  - Rust crate: `surrealdb` v3，features `kv-mem` / `kv-rocksdb`

> [!IMPORTANT]
> **后端选择（三级 RAM 降级，`cmd/polaris/boot_substrate.go initSurrealStore`）**
> - **<2GB**: 跳过 SurrealDB，FeatureSurrealDBCore 禁用
> - **2-4GB**: 强制 `kv-mem`（覆盖配置），重启丢数据，由 SQLite Outbox 投影恢复（§2.5）
> - **≥4GB（默认）**: `configs/defaults.toml [cognition] surreal_backend`（默认 `"rocksdb"`），落盘 `~/.polaris/data/surreal.db`，RSS ~200MB
> - **≥8GB**: 额外自动开启 workerThreads

## 11. 四层记忆 → 存储绑定

| 记忆层 | 物理存储 | 持久化 |
|--------|----------|--------|
| L0 Working Memory | 进程内 ContextWindow(Slice) + ScratchPad(sync.Map) [Tier-0] + Immutable Core | 跨 session 笔记经 NotesStore（SQLite `notes` 表）；工作上下文本身进程内 |
| L1 Episodic Memory | [Storage-SQLite] `episodic_events` + [Storage-SurrealDB-Core] embedding 列 + 时序 B-tree | 是 |
| L2 Semantic Memory | [Storage-SQLite] 主存储（邻接表 entity + relation）；[Storage-SurrealDB-Core] 图遍历 + KNN | 是 |
| L3 Procedural Memory | [Storage-SurrealDB-Core] skill_id→SkillBytes + 语义检索 | 是 |
## 15. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| SQLite 文件损坏 | fail-stop + CRITICAL 告警，提示修复/重建 | 从 EventLog 备份重建 |
| SurrealDB-Core 侧车故障 | 降级到 SQLite FTS5 兜底 | 引擎恢复后自动切回 |
| Outbox 积压（>500 pending） | 暂停非关键 connector + WARN | 降至 <200 恢复 |
| 磁盘空间不足 | L1 压缩冷数据 / L2 停止摄入非关键源 / L3 拒绝写入 | 空间恢复后逐步重开 |
| SQLite WAL 过大 | 自动 checkpoint | — |

## 默认参数

阈值与重评触发条件: `spec/state.yaml §thresholds.m2_storage`。

## 16. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M1 Inference | Embedding API（Dimension 运行时获取、OnlineReindexer 触发） | M1 §9 |
| M4 Agent Kernel | EventLog Append/GetEvents（崩溃恢复回放源） | M4 §8 |
| M5 Memory | 四层记忆 → Store 引擎绑定、episodic_events 派生投影 | M5 §1, §3 |
| M10 Knowledge RAG | doc_nodes/chunks/summaries 三层索引存储、Outbox Worker 共用 | M10 §3.2 |
| M11 Policy Safety | CredentialVault 为前置屏障（StorageFabric.Open() 须在 Init() 之后） | M11 §5.2 |
| 接口定义 | Store/Transaction/Iterator/MutationIntent/DatabaseWriter | internal/protocol/interfaces_store.go, internal/store/ |
| 全局字典 | HE-Rule-6 State-in-DB、EventLog/MutationBus/Idempotency-Key 定义 | 00-Global-Dictionary §6 |
| DDL | 全部 DDL（001~024 + 029~048，共 44 份，025~028 为刻意预留跳号），权威目录 `internal/protocol/schema/` | internal/protocol/schema/ |
| DDL 约束 (entities) | `UNIQUE(name, type)` 位于 `004_semantic_memory.sql`，支持 GraphWriter OpUpsert 幂等 ON CONFLICT（M10 §2.7） | internal/protocol/schema/004_semantic_memory.sql |
| tasks 表新增列 | `suspend_reason`（hitl / provider_exhausted / killswitch）、`provider_suspended_count` | M4 §8, M11 §5.1 |
| 时序图 | EventLog 写入与崩溃恢复全流程 | DIAGRAMS.md#eventlog |

---

## 已修复实现缺口记录

| 文件 | 问题摘要 | 修复方案 |
|------|---------|---------|
| `internal/observability/` (TokenBurnRate) | `baselineP95` 零初始化导致误熔断 | 冷启动守卫 `baselineP95 <= 0` 返回 Normal |
| `internal/store/` (SchemaManager) | 迁移事务 nil panic | 独立事务 + db != nil 守卫 |
| `internal/store/` (HybridRetriever) | 余弦相似度除以范数平方积 | 修正为标准余弦公式 |
