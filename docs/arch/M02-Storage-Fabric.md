# 模块 2: Storage Fabric

> 多存储引擎并存，全部可嵌入。Go 编排/接口/Outbox Worker/Schema Migration，Rust 侧车热路径引擎 FFI。
> [HE-Rule-3] [HE-Rule-5] [HE-Rule-6] [Tier-0-Limit] [Day0-ColdStart] [Phase0-Bootstrapping]
> **§跳读**: 0-bis:6 职责 / 0-ter:17 不变量速查 / 1:28 接口层 / 2:52 EventLog / 2.6:167 tasks表 / 3:175 容量 / 4:210 Workspace / 5:254 SchemaManager / 6:266 Reindexer / 7:280 Go↔Rust FFI / 8:304 连接池 / 9:312 多写者 / 10:323 引擎速查 / 11:337 四层记忆映射 / 15:343 428(SOFT)降级 / 16:355 依赖
## 0-bis. 职责边界

- M2 **是**: 多引擎统一抽象接口（Store interface） | M2 **不是**: 具体引擎的内部实现（引擎自身负责）
- M2 **是**: EventLog 真相源存储（events 表 append-only） | M2 **不是**: 事件语义解释者（各业务模块）
- M2 **是**: 跨引擎 Outbox 异步投影 + MutationBus 单写者 | M2 **不是**: 跨引擎 ACID 保证（嵌入式不可实现）
- M2 **是**: SQL Schema Migration 管理 | M2 **不是**: 业务缓存策略（M5 自行管理）
- M2 **是**: 多引擎间数据路由（Storage-Router） | M2 **不是**: 索引构建逻辑（M10/KB 自行管理）
- M2 **是**: DDL 物理 schema 权威定义 | M2 **不是**: 跨模块接口契约定义（`internal/protocol/interfaces.go`）

---

## 0-ter. 不变量速查表

- 编号: inv_M2_01 | 不变量: events 表仅 INSERT，禁止 UPDATE/DELETE | 验证方式: DDL 权限 + CI `eventlog_append_only` 测试
- 编号: inv_M2_02 | 不变量: 所有状态变异经 MutationBus DatabaseWriter 单写者串行化 | 验证方式: CI `mutation_bus_lint` 静态扫描
- 编号: inv_M2_03 | 不变量: 跨引擎不承诺 ACID——走 outbox + 幂等键 + 最终一致 | 验证方式: 集成测试验证幂等重放
- 编号: inv_M2_04 | 不变量: embed_model_version 是一等字段——每向量携带，跨版本检索走 OnlineReindexer backfill | 验证方式: DDL CHECK 约束
- 编号: inv_M2_05 | 不变量: 死信记录绝不静默丢弃——超 max_attempts 进 dead 状态 + 告警 + 人工介入 | 验证方式: outbox.dead.count >0 告警
- 编号: inv_M2_06 | 不变量: audit_log 表 DDL 触发器禁止 UPDATE/DELETE（append-only 硬约束） | 验证方式: CI `audit_integrity` 测试

---

## 1. 统一接口层

### 1.1 Store 接口

契约权威源 `internal/protocol/interfaces.go` `Store` / `Transaction` 接口。所有引擎适配器须实现。

### 1.2 [Storage-Router]

实现见 `internal/store/`（StorageRouter）。按优先级依次匹配路由规则，全部未命中时回落 SQLite。SurrealDB FFI 加载失败时路由表置空，所有请求直连 SQLite。

### 1.3 路由规则表

- 数据类型: Agent 会话状态/配置 | 访问模式: 随机读写 | 延迟要求: <1ms | 存储: [Storage-SQLite]
- 数据类型: Embedding 向量 | 访问模式: 批量写 + KNN 读 | 延迟要求: <5ms | 存储: [Storage-SurrealDB-Core]
- 数据类型: 事件日志 | 访问模式: Append-only 写 + 时序扫描 | 延迟要求: <100us 写 | 存储: [Storage-SQLite] WAL
- 数据类型: 技能缓存（热点） | 访问模式: 高频读 | 延迟要求: <10us | 存储: [Storage-SurrealDB-Core]
- 数据类型: 知识图谱遍历 | 访问模式: 随机多跳读 | 延迟要求: <10ms | 存储: [Storage-SurrealDB-Core]（Rust FFI via purego，原生多跳图遍历）
- 数据类型: 知识片段 (knowledge) / 全文检索 (fulltext) | 访问模式: 批量写 + ad-hoc 查询 | 延迟要求: <50ms | 存储: [Storage-SurrealDB-Core]
- 数据类型: 路由表/元数据 | 访问模式: 高频读 + 低频写 | 延迟要求: <1us | 存储: sync.Map / [Storage-SurrealDB-Core]

> **部署说明（三级 RAM 降级，`boot_substrate.go initSurrealStore`）**: <2GB → 跳过 SurrealDB；2-4GB → 强制 `mem`（无持久化）；≥4GB → 使用配置后端（默认 `rocksdb`，持久化落盘，~200MB RSS 开销）；≥8GB → 自动开启 workerThreads。

---

## 2. EventLog — 真相源

[HE-Rule-6] 所有状态持久化落盘。EventLog 是系统唯一真相源，所有派生引擎状态可从 events + outbox 重建。

### 2.1 events 表（EventLog 物理存储）

DDL 和索引定义见 `internal/protocol/schema/001_events.sql`。

关键设计决策:
- 序列化: Protobuf（Schema 演化，Go↔Rust 同 .proto 生成，~60-80% 体积缩减）
- ID: ULID（时间有序，UUID v4 破坏时间局部性）
- Offset: AUTOINCREMENT（NTP 漂移/时钟回退免疫）
- M5 `episodic_events` 为此表的派生投影表（记忆检索优化），通过 `idempotency_key` 关联

接口: Read(ctx, fromOffset, maxBatch) → (events, error) | Subscribe(ctx, fromOffset) → (chan Event, error)

### 2.2 EventWriteBuffer — MPSC 批量写入缓冲

消除多 Agent 并发写 SQLite 的写锁争抢。Agent Emit → channel (<1us) → 批量 flush → 构造 MutationIntent → DatabaseWriter 串行 INSERT。EventWriteBuffer 为纯缓冲层，不持有独立写路径。

实现见 `internal/store/`（EventWriteBuffer）：非阻塞投递，channel 满时指数退避重试（5 次，10ms→2s），严禁同步写兜底；Critical 事件走 PriorityFlush 零丢失；批量 flush（cap=64, 100ms ticker）前执行租约二次校验。生命周期由 StorageFabric 内聚管理，不依赖 M8 Supervisor Tree。

持久性: 常规事件 SIGKILL 最多丢 100ms 窗口。SIGTERM 通过 §2.4 handleSignals 排空。Critical 事件 EmitCritical 经 DatabaseWriter PriorityFlush 零丢失。

### 2.3 MutationBus + DatabaseWriter — 通用状态变异串行化

实现见 `internal/store/`（MutationBus + DatabaseWriter）。核心文件：`mutation_bus.go`（接口+投递）、`mutation_bus_execute.go`（DatabaseWriter 执行循环）、`event_buffer.go`（EventWriteBuffer MPSC 缓冲）、`embedding_batcher.go`（向量批处理）、`reranker.go`（重排序）。

禁止业务 goroutine 直接 BEGIN IMMEDIATE 写 SQLite。所有状态变异通过 MutationBus 投递变异意图（含表名、操作类型、优先级、组合 ID），DatabaseWriter 单一 goroutine 串行执行。

关键语义：
- 普通投递：channel 投递 + 指数退避重试（10ms→2s），严禁同步执行兜底
- ETL 批量投递：微事务分片（MaxRowsPerTx=50），分片间主动让出调度
- 组合投递：同 GroupID 成员在单事务内原子提交（全成功或全失败）
- 执行循环：batch + ticker 消费，崩溃后自动重启
- 阈值参数见 `spec/state.yaml §m2_storage`

覆盖范围 — 以下路径必须走 MutationBus (CI `mutation_bus_lint` 静态扫描强制):
- M5 NotesStore.Set/Delete
- M2 OutboxWorker 状态更新
- sys_config/schema_versions (仅 SchemaManager 维护模式豁免)

### 2.4 优雅关闭

收到 SIGTERM/SIGINT 后：取消全局 context → 5s 超时内排空 EventWriteBuffer（超时则 WARN + 丢弃）→ os.Exit(0)。

### 2.5 Outbox 模式 — 跨引擎最终一致

嵌入式跨引擎 ACID 不可实现。以 EventLog 为真相源 + Outbox 投影 + 幂等键。

DDL 见 `internal/protocol/schema/002_outbox.sql`。

业务写入通过 CompositeMutationIntent 保证同一 SQLite 事务内原子提交。

**CompositeMutationIntent 执行边界**:
events 表统一走 DatabaseWriter 单写者；EventWriteBuffer 为纯批量缓冲层。
路径: Agent Emit → EventWriteBuffer.ch (<1μs) → 100ms 批量 flush → MutationIntent{Priority=PriorityFlush} → DatabaseWriter 串行 INSERT。
event 写入与业务表写入同 DatabaseWriter 事务原子提交。CompositeMutationIntent 天然跨 events + 业务表。
统一单写者是 [HE-Rule-6] 唯一路径（崩溃恢复时 EventLog 与业务状态因果完全一致）。

OutboxWorker（`internal/store/outbox_worker.go`）批量拉取待处理记录：主查询取 cursor 之后的 pending/failed 记录，补充查询捕获 cursor 之前遗漏的失败记录（最多 batchSize/4），防止历史失败记录被遗漏。

**Handler 注册**：`RegisterHandler(taskType, handler, checker...)` 注册各目标引擎处理器（如 `m10_graph_build`/`episodic`），可选传入版本高水位检查器。消费循环按 `target_engine` 路由到对应 handler。

**Cursor 持久化**：游标持久化到 `sys_config` 表（key=`outbox_cursor`），`loadCursor` 启动时恢复，`saveCursor` 每批提交后原子 CAS 更新，保证重启后不漏消费。

**指数退避**：失败记录 backoff = `(1 << attempts) * 5000ms`，`next_retry_at` 业务主动设置的最早执行时间下界与退避共同生效（取最大值）。

Idempotency Key 格式: `{target_engine}:{entity_type}:{entity_id}:{operation}:{version}`

版本高水位拦截: 所有目标引擎写入前校验 existing_version >= incoming_version → 丢弃并返回 ErrVersionStale。将单消息幂等升级为版本偏序幂等。

**Outbox 表关键列说明**（DDL 权威定义见 `internal/protocol/schema/002_outbox.sql`，以下为文档层声明）:

- 列: `id` | 类型: INTEGER PK | 语义: AUTOINCREMENT，全局单调递增游标
- 列: `target_engine` | 类型: TEXT | 语义: 目标消费 handler，如 `m4_provider_recovery`/`m10_graph_build`
- 列: `payload` | 类型: TEXT | 语义: JSON/msgpack 业务负载
- 列: `idempotency_key` | 类型: TEXT UNIQUE | 语义: 防重复投递
- 列: `status` | 类型: TEXT | 语义: pending/processing/done/failed(指数退避待重试)/dead(毒丸)
- 列: `attempts` | 类型: INTEGER DEFAULT 0 | 语义: 已尝试次数，`>= max_attempts` 置 dead
- 列: `crash_recovery_count` | 类型: INTEGER DEFAULT 0 | 语义: Poison Pill 计数，`>= 3` 直接置 dead
- 列: `next_retry_at` | 类型: TEXT（nullable） | 语义: **业务级最早可处理时间**（UTC ISO 8601）。业务 handler 显式设置，独立于指数退避。例: GraphRAG LLM 日预算耗尽时设次日 00:00:00 UTC。fetchBatch 主查询和迟提交补偿均检查（`next_retry_at IS NULL OR next_retry_at <= now`），防预算恢复前无效扫描
- 列: `created_at` | 类型: TEXT | 语义: 投递时间 UTC
- 列: `updated_at` | 类型: TEXT | 语义: 最后状态变更时间 UTC

注：`next_retry_at` 与指数退避计算的下次执行时间（由 `attempt_count + 退避算法` 计算，不持久化为独立列）语义不同——前者是业务主动设置的"最早可执行时间下界"，后者是失败重试的自动退避时间。两者共同生效：fetchBatch 取 `max(business_next_retry_at, backoff_time)` 决定是否拉取。

Poison Pill 毒丸驱逐: Worker 执行 FFI 前原子递增 crash_recovery_count: `UPDATE outbox SET status='processing', crash_recovery_count = crash_recovery_count + 1 WHERE id = ?`。crash_recovery_count >= 3 → 直接标记 dead，阻断确定性崩溃循环。

卡死 processing 恢复: Worker 启动时 `UPDATE outbox SET status='pending' WHERE status='processing'`。Janitor 每 5 分钟恢复 `processing AND updated_at < now() - 300s`。

已完成记录清理: status IN ('done','dead') AND created_at < now() - 7d，Janitor 每 6h 批量 DELETE (<=1000行/批 + Gosched)。

监控: outbox.pending.count | outbox.lag.seconds | outbox.dead.count (>0 告警)

Embedding 维度运行时获取: 所有向量维度由 M1 `Embedder.Dimension()` 运行时返回，禁止编译期硬编码。维度变更触发 OnlineReindexer。维度不匹配返回 ErrDimensionMismatch，调用方降级 BM25/FTS5。

---

## 2.6 tasks 表 — Agent 任务状态核心列

DDL 权威定义见 `internal/protocol/schema/007_tasks.sql`。以下为文档层声明，覆盖所有历史迁移后的最终列集合。

- 列: `task_id` | 类型: TEXT PK | 语义: ULID，全局唯一任务标识
- 列: `session_id` | 类型: TEXT | 语义: 所属 Session，关联 events 表
- 列: `status` | 类型: TEXT | 语义: Pending/Claimed/Executing/Done/Failed/Suspended/Compensating
- 列: `priority` | 类型: INTEGER DEFAULT 1 | 语义: 0=用户交互 / 1=前台辅助 / 2=后台优化 / 3=最低（Auto-Curriculum）
- 列: `claimed_by` | 类型: TEXT（nullable） | 语义: 认领该任务的 agentID；nil 表示未认领
- 列: `claimed_at` | 类型: TEXT（nullable） | 语义: 认领时间 UTC
- 列: `expires_at` | 类型: TEXT（nullable） | 语义: 租约到期时间 UTC；Reaper 检测此字段驱逐过期任务
- 列: `version` | 类型: INTEGER DEFAULT 0 | 语义: 乐观锁版本计数；CAS Claim/BeginExecution/Reaper 均递增
- 列: `replan_count` | 类型: INTEGER DEFAULT 0 | 语义: ReplanGuard 计数；>= MaxReplanAttempts (`spec/state.yaml §m4_kernel.max_replan_attempts`) → S_FAILED
- 列: `depends_on` | 类型: TEXT（nullable） | 语义: JSON array of task_id，Macro-DAG 前驱依赖
- 列: `suspend_reason` | 类型: TEXT（nullable） | 语义: 挂起原因标记，枚举: `hitl` / `provider_exhausted` / `killswitch`（**added: #23 audit fix**）
- 列: `pii_vault_blob` | 类型: TEXT（nullable） | 语义: SessionPIIVault.SuspendSnapshot 落盘的加密 blob（AES-256-GCM，key 由 M11 CredentialVault.persistent_key 派生）；恢复后由 RestoreFromSnapshot 消费并 SecureZero（**added: #23 audit fix**）
- 列: `provider_suspended_count` | 类型: INTEGER DEFAULT 0 | 语义: provider_exhausted 自动唤醒计数；> 5 触发 [ESCALATE] + HITL，转 HITL-Suspended TTL 管理（**added: #23 audit fix**）
- 列: `created_at` | 类型: TEXT | 语义: 任务创建时间 UTC
- 列: `updated_at` | 类型: TEXT | 语义: 最后状态变更时间 UTC

注：`pii_vault_blob`、`suspend_reason`、`provider_suspended_count` 三列在 #23 修复中引入，解决 SessionPIIVault 跨 Provider 熔断的状态持久化问题。实现细节见 M4 §8（ErrAllProvidersFailed 专项处理）和 M11 §5.1（SessionPIIVault）。

---

## 3. EventLog 容量预算与冷热归档策略

### 3.1 Hot/Warm/Cold 三级存储

| 层级 | 保留期 | 存储引擎 | 查询能力 | 触发条件 |
|------|--------|---------|---------|---------|
| Hot | 当前 Session | SQLite events 表 | 全字段索引查询 | events 表写入即时 |
| Warm | `spec/state.yaml §m2_storage.eventlog_hot_days` | SQLite events 表 | 全字段索引查询（低优先级） | 越过 hot_days 标记 `archived` 软删除 |
| Cold | 永久归档 | Parquet 文件（`data/cold/events/`） | 仅 DuckDB 回读 | 越过 `eventlog_warm_days` + 磁盘水位 <20% 触发 |

### 3.2 磁盘水位触发归档

D1 (安全) 触发: 磁盘使用率 >70% → 自动触发 Cold 归档（Hot→Warm→Cold 逐级淘汰）。
D2 (性能) 触发: Hot 表行数 >100 万或空间 >500MB → 自动触发 Warm 压缩。
D3 (紧急) 触发: 磁盘使用率 >90% → 淘汰已归档→Parquet 的 Cold 备份（保留 hash chain 引用）。

### 3.3 容量预算归档表

| 表 | HT0 稳态 (MB) | HT0 峰值 (MB) | 备注 |
|----|-------------|-------------|------|
| events（EventLog） | ~100 | ~200 | ~50-100 事件/Session, 平均~1KB/行 |
| outbox | ~10 | ~30 | 临时投影队列 |
| episodic_events（向量投影） | ~80 | ~150 | embeddings ~768d, 压缩率 ~5x |
| decision_log | ~20 | ~50 | ~10 条决策/Session |
| tasks | ~10 | ~20 | Agent 任务状态 |
| workspace 文件系统 | ~50 | ~200 | 爬取结果等中间物 |
| 索引 + 临时 | ~40 | ~100 | 向量索引 + FTS5 + 迁移备份 |
| **EventLog 合计** | **~310** | **~750** | 占 M2 总预算 310MB (HT0 steady) 的主体 |

### 3.4 归档实现接口【计划中，未实现】

EventLog Archiver 设计为 M2 后台周期性 Worker：按 `created_at < now - 30d` 批量导出 → Parquet（zstd 压缩）→ `data/cold/events/{year}/{month}/` → sha256 防重复 → 原始行删除（需 ESCAPE 审批）。Tier 0 默认禁用，由 M3 OSMemoryGuard 磁盘水位信号激活。

---

## 4. WorkspaceManager — 重型中间物文件系统

大规模爬取结果、AST dump、diff patch、二进制文件不入 SQLite（Blob 膨胀），不入 Working Memory（[Tier-0-Limit]）。Working Memory 仅持有路径+摘要。物理路径：`~/.polarisagi/polaris/workspace/<task_id>/`，权限 0700。

实现见 `internal/store/`（WorkspaceManager）：

- 创建任务隔离目录并注册 manifest（幂等）；启动时自动创建根目录。
- 文件注册：将文件元数据写入 manifest，供配额累计使用。
- 写入前配额校验：累积占用 + 待写大小超过 maxSize（Tier 0 = 500MB）时拒绝。
- GC：物理删除超过 7 天的非活跃工作区；跳过仍活跃的任务工作区，防止删除正在运行的持久战任务数据。
- DirPath：返回任务工作区物理路径（不创建）。

写入实时拦截：workspace_write 前 CheckQuota → 超限返回 ErrQuotaExhausted。每 30s OTel 探针上报可用空间，<100MB → CRITICAL + 暂停所有 workspace_write。

**Workspace GC**：M13 ResourceReaper 每日 04:00 触发，回收 >7 天且关联 Task 状态为 Done/Failed 的 workspace。紧急模式：写入拦截触发 → Reaper.RunNow()，跳过定时。

**Workspace 绝对生命周期上限防线（防永久泄漏）**:

  _HITL-Suspended 超时_ (`suspend_reason='hitl'`, 默认 TTL=30 天可配):
    - 提前 5 天: ResourceReaper 写 `hitl_suspension_expiry_warning` WARN 审计 + 操作员通知
    - 到期: (a) 清零 pii_vault_blob（PII 先于一切删除）→ (b) MutationBus 置 S_FAILED + 写 `suspended_hitl_timeout_expired` → (c) HITL 通知（M13）→ (d) 之后 7 天走正常 GC

  _KillSwitch-Suspended_: 无 TTL（等 unseal 自动恢复）。但磁盘 <100MB CRITICAL 且 workspace UpdatedAt >7 天 → 打包 `~/.polarisagi/polaris/archive/<task_id>_<timestamp>.tar.zst` 删原目录，保留 Blackboard 元数据。unseal 时 M13 检查 archive 存在 → 先解压再恢复任务。归档上限 10GB（LRU 删最老 + WARN）。

  _Dead-letter Pending_: `status=Pending` 且 Outbox max_attempts 耗尽 (`dead_letter=true`) 且 UpdatedAt+7d>now → 直接 S_FAILED + GC workspace。

  _provider_exhausted-Suspended_: 无 TTL。M1 CircuitBreaker 恢复 (§7.3) 触发自动唤醒。`provider_suspended_count > 5` 已 ESCALATE→HITL，转 HITL-Suspended TTL 管理。

  配置参数（`internal/config/immutable_constants.go` 之外可调）:
    `hitl_suspension_ttl_days` = 30 (`polaris config set ... N`，最小 7 天)
    `archive_max_size_gb` = 10

**Workspace 静态加密**: 外部 Connector (M10) 拉取的原始文件落盘前 AES-256-GCM 加密（key 由 M11 CredentialVault.persistent_key 派生）。强制加密: `[TaintLevel]` ≥ `[Taint-Medium]`；可选跳过: `[Taint-Low]`/`[Taint-None]`（系统自生成 / 用户本地代码，省 CPU）。密钥与 M11 SafeString HMAC 共享同一 persistent_key。

VFS 引用计数 + SQLite Trigger 自动回收: 热表大型载荷 (>4KB) 不存入 B-Tree 页，写入 VFS 文件 (`~/.polarisagi/polaris/vfs/{sha256_prefix}/{uuid}.blob`)，热表仅存 `vfs_ref` 指针。4KB 热行硬限防 B-Tree 页缓存血崩。

```sql
sys_vfs_references: vfs_ref TEXT PK, ref_count INTEGER
```

BEFORE DELETE trigger 自动递减引用计数，引用归零入队 GC。4KB 硬限 CI `migration_lint` 强制执行。

---

## 5. Schema 迁移策略

**当前阶段（上线前）**：Schema 变更直接修改 `internal/protocol/schema/NNN_*.sql` 原始 DDL 文件，删库重建（`rm ~/.polarisagi/polaris/data/polaris.db`）。禁止以 ALTER TABLE/ADD COLUMN 补丁文件打补丁。

**上线后**：新增编号迁移文件（ALTER TABLE / 数据迁移），由 `internal/store/`（SchemaManager）负责：按版本升序执行，每条迁移在独立事务内运行（失败自动回滚），前后向 `sys_config` 写入状态标记（idle / in_progress / completed）。崩溃恢复：启动时检测到 `in_progress` 则拒绝启动，要求操作员重置后重启。

**兼容策略**：新字段一律 NULLABLE 或有 DEFAULT；降级不执行 Down，旧代码忽略新列。

冷存储 EventLog 重放: >30 天记录归档 Parquet。重放优先 M4 FSM Snapshot → Snapshot 不可用则 DuckDB 从 Parquet 回读。

---

## 6. OnlineReindexer — 零停机向量索引重建

实现位置：`internal/memory/`（M5 记忆层，非 substrate）。
对抗 embedding 空间漂移（3-6 月周期）。基于 `embed_model_version` 字段检测未索引条目，批量后台回填（batchSize=50，best-effort）。

- **回填**：新版本索引在后台构建，throttle ≤100 rows/s，同时 Outbox 双写保证增量数据一致
- **切换**：新旧索引版本指针原子更新，旧索引保留 7 天供回退
- **降级**：新索引 Recall@5 ≤ 旧索引 90% → ABORT，保留旧索引，写 WARN 日志
- **崩溃恢复**：启动时扫描 `reindex_progress` 元数据，按状态（backfilling/swapping）恢复或回滚

监控: polaris_reindex_progress (0.0-1.0 gauge) | polaris_reindex_rows_per_second (gauge) | polaris_surreal_index_versions (gauge, 活跃索引版本数)

---

## 7. Go↔Rust FFI 集成边界

> Rust FFI 编码级约定（purego ABI、文件组织、Cargo.toml 约束）见 docs/specs/02-Rust-FFI.md。

Rust 侧:
- 所有跨 FFI 函数必须 catch_unwind — panic 不可跨 FFI
- 返回 i32 错误码, 0=成功, 错误详情走 thread-local last_error()
- cbindgen 自动生成 C 头文件, CI 提交

内存所有权:
- Go→Rust 短生命周期入参由 Go 分配/释放
- Rust→Go 大批量返回值由 Go 预分配 buffer, Rust 写入 (避免跨 FFI 分配)

编译: CI 矩阵预编译三平台 Rust 静态库 (.a/.lib), Go build 不触发 Rust 编译

错误: Rust tracing → FFI 桥接 Go slog, 不走 stdout/stderr

FFI 崩溃分层防御:
  L1: CI ASAN/Valgrind 检测 C 内存错误
  L2: debug.SetPanicOnFault(true) 将 SIGSEGV 转 Go panic → suture 可捕获
  L3: OS systemd/launchd Restart=always + EventLog 回放恢复

---

## 8. SQLite 连接池

当前实现：全局单连接模式，读写共用同一连接，与 MutationBus 单写者约束对齐，禁止并发写。实现见 `internal/store/`。

Outbox Worker 与 Agent 查询共用同一连接，由 MutationBus 串行化保证无竞争。

---

## 9. 多写者防御层级 (WAL 模式)

- 层: L0 | 机制: EventWriteBuffer MPSC 批量缓冲 | 阈值: `spec/state.yaml §m2_storage.mutation_bus_channel_cap` / `max_batch_size` / `ticker_interval_ms`
- 层: L1 | 机制: PRAGMA busy_timeout | 阈值: `spec/state.yaml §m2_storage.sqlite_busy_timeout_ms`
- 层: L2 | 机制: PRAGMA wal_autocheckpoint | 阈值: `spec/state.yaml §m2_storage.wal_checkpoint_pages`
- 层: L3 | 机制: WAL 大小监控 + PASSIVE/RESTART checkpoint | 阈值: WAL>50MB PASSIVE, >200MB RESTART, 30s 检查
- 层: L3.5 | 机制: WAL 临界截断 (sqlite3_interrupt + TRUNCATE) | 阈值: WAL>500MB, 1s 检查
- 层: L4 | 机制: 读事务 ctx 超时 + 分页释放读锁 | 阈值: ctx.WithTimeout(5s)

---

## 10. 引擎选择速查（2026 极简三轴架构）

- 引擎: **[Storage-SQLite]** (modernc.org/sqlite, 纯 Go CGO-Free)
  - 用途: 系统控制轴。唯一的绝对真相源 (EventLog)、任务状态机、ACID 队列 (Outbox)。零 FFI 开销，最高稳定性。
- 引擎: **[Storage-SurrealDB-Core]** ([surrealdb](https://github.com/surrealdb/surrealdb) crate，Rust cdylib via purego, CGO-Free FFI)
  - 用途: 认知检索轴。SurrealDB 嵌入式模式原生提供 KV + HNSW 向量索引 + 有向图遍历 + BM25 全文检索，单一 crate 四轴闭环。决策与被驳方案（Qdrant+neo4j+ES / 仅 SQLite 自建 / BoltDB / 全 Rust 重写 / rust-rocksdb 直接使用）见 [ADR-0010](./decisions/ADR-0010-surrealdb-cognitive-storage.md)。
  - **Rust crate**: `surrealdb = { version = "3", features = ["kv-mem", "kv-rocksdb"] }`
  - > [!IMPORTANT]
    > **后端选择策略（三级 RAM 降级，`boot_substrate.go initSurrealStore`）**
    > - **<2GB**: 跳过 SurrealDB，FeatureSurrealDBCore 禁用。
    > - **2-4GB**: 强制 `kv-mem`，覆盖配置文件，进程重启数据丢失，由 SQLite Outbox 投影恢复（§2.5）。
    > - **≥4GB（默认路径）**: 使用 `configs/defaults.toml [cognition] surreal_backend`（默认值 `"rocksdb"`），持久化落盘 `~/.polarisagi/polaris/data/surreal.db`，RSS ~200MB。
    > - **≥8GB**: 额外自动开启 workerThreads，提升并发处理能力。
    > - **surreal-mem**: 仅在 RAM 2-4GB 时自动降级，或显式配置 `surreal_backend = "mem"` 时使用。
- 引擎: **[Storage-Native]** (纯 Go 内存原生结构)
  - 用途: 热缓存轴。纯内存态的 L0 Working Memory，使用 `sync.Map` 和原生切片，满足 8GB 内存 (Tier-0) 约束。

## 11. 四层记忆 → 存储绑定

- 记忆层: L0 Working Memory | 物理存储: 进程内原生 ContextWindow(Slice) + ScratchPad(sync.Map) [Tier-0] + Immutable Core | 持久化: 跨 session 笔记经 NotesStore（SQLite `notes` 表）持久化，工作上下文本身进程内
- 记忆层: L1 Episodic Memory | 物理存储: [Storage-SQLite] `episodic_events` 表 + [Storage-SurrealDB-Core] embedding 列 + 时序 B-tree | 持久化: 是
- 记忆层: L2 Semantic Memory | 物理存储: [Storage-SQLite] 主存储（邻接表 entity + relation）；[Storage-SurrealDB-Core] 负责图遍历 + KNN 向量检索 | 持久化: 是
- 记忆层: L3 Procedural Memory | 物理存储: [Storage-SurrealDB-Core] skill_id→SkillBytes + [Storage-SurrealDB-Core] | 持久化: 是
## 15. 降级与失败模式

- 故障场景: SQLite 文件损坏 | 降级路径: fail-stop + CRITICAL 告警，提示用户修复/重建 | 恢复策略: 从 EventLog 备份重建
- 故障场景: SurrealDB-Core 认知侧车故障 | 降级路径: 降级到 SQLite FTS5 兜底 | 恢复策略: 引擎恢复后自动切回
- 故障场景: Outbox 积压（>500 pending） | 降级路径: 暂停非关键 connector + WARN | 恢复策略: 积压降至 <200 恢复正常
- 故障场景: 磁盘空间不足 | 降级路径: L1: 压缩冷数据 / L2: 停止摄入非关键源 / L3: 拒绝写入 | 恢复策略: 空间恢复后逐步重开
- 故障场景: SQLite WAL 文件过大 | 降级路径: 自动 checkpoint | 恢复策略: —

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m2_storage`。

## 16. 跨模块依赖与契约

- 关联模块: M1 Inference | 关键契约: Embedding API（Dimension 运行时获取、OnlineReindexer 触发） | 位置: M1 §9
- 关联模块: M4 Agent Kernel | 关键契约: EventLog Append/GetEvents（崩溃恢复回放源） | 位置: M4 §8
- 关联模块: M5 Memory | 关键契约: 四层记忆 → Store 引擎绑定、episodic_events 派生投影 | 位置: M5 §1, §3
- 关联模块: M10 Knowledge RAG | 关键契约: doc_nodes/chunks/summaries 三层索引存储、Outbox Worker 共用 | 位置: M10 §3.2
- 关联模块: M11 Policy Safety | 关键契约: CredentialVault 为前置屏障（StorageFabric.Open() 须在 Init() 之后） | 位置: M11 §5.2
- 关联模块: 接口定义 | 关键契约: Store/Transaction/Iterator/MutationIntent/DatabaseWriter | 位置: internal/protocol/interfaces.go, internal/store/- 关联模块: 全局字典 | 关键契约: HE-Rule-6 State-in-DB、EventLog/MutationBus/Idempotency-Key 定义 | 位置: 00-Global-Dictionary §6
- 关联模块: DDL | 关键契约: 全部 DDL（001_events 至 028_apps，共 25 份，权威目录 `internal/protocol/schema/`） | 位置: internal/protocol/schema/
- 关联模块: DDL 约束 (entities 表) | 关键契约: `UNIQUE(name, type)` 约束位于 `004_semantic_memory.sql`，支持 GraphWriter OpUpsert 的幂等 ON CONFLICT 语义（M10 §2.7） | 位置: internal/protocol/schema/004_semantic_memory.sql
- 关联模块: tasks 表新增列 | 关键契约: `pii_vault_blob TEXT`（nullable）—— SessionPIIVault.SuspendSnapshot 落盘字段（M11 §5.1）; `suspend_reason TEXT`（nullable）—— 区分 hitl / provider_exhausted / killswitch; `provider_suspended_count INTEGER DEFAULT 0` | 位置: M4 §8, M11 §5.1
- 关联模块: 时序图 | 关键契约: EventLog 写入与崩溃恢复全流程 | 位置: DIAGRAMS.md#eventlog

---

## 已修复实现缺口记录

| 文件 | 问题摘要 | 修复方案 |
|------|---------|---------|
| `internal/observability/` (TokenBurnRate) | `baselineP95` 零初始化导致误熔断 | 冷启动守卫 `baselineP95 <= 0` 返回 Normal；显式设置基线 |
| `internal/store/` (SchemaManager) | 迁移事务 nil panic | 独立事务执行，db != nil 守卫 |
| `internal/store/` (HybridRetriever) | 余弦相似度除以范数平方积 | 修正为标准余弦公式 |
