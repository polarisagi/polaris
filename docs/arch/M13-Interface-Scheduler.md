# 模块 13: Interface & Scheduler

> 对外: CLI + HTTP/SSE + MCP + Web UI; 对内: 任务队列 + 定时任务 + HITL
> Go; [HE-Rule-1]; [Tier-0-Limit]; [Phase0-Bootstrapping]
> **§跳读**: 0-bis:6 职责 / 0-ter:21 不变量速查 / 1:34 对外接口 / 2:286 对内调度 / 3:386 MCP / 6:397 (SOFT)降级 / 7:422 跨模块契约 / 8:439 Web UI 规约 / 8.6:插件聚合市场DB+流 / 8.7:自动化中心DB+流+工作流 / 8.8:电脑操控权限+Preferences
## 0-bis. 职责边界

| M13 **是** | M13 **不是** |
|-----------|-------------|
| 对外接口层（CLI REPL + HTTP REST + SSE + Web UI） | 业务逻辑执行（由各模块负责） |
| 对内调度（TaskQueue + Cron + ResourceReaper） | 任务分解与编排（那是 M8） |
| HITL 审批网关（HITLGateway + Notifier） | 审批策略制定（那是 M11 [ESCALATE]） |
| TrafficSplitter 流量分发执行（percent 控制） | 流量切换决策（那是 M9 ProgressiveRollout） |
| ResourceGovernor 准入控制（三级降级联合执行） | 内存压力检测（那是 M3 OSMemoryGuard） |
| Sealed/Unsealed 服务器状态管理 | KillSwitch 阶段触发（那是 M11） |
| 对外 API 认证（Session Token + API Key） | 凭证存储（那是 M11 CredentialVault） |
| EgressGateway Provider 域名白名单预检 | 网络连接安全（委托 M11 SafeDialer.DialContext 完整执行） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M13_01 | 默认绑定 127.0.0.1——远程绑定须显式 + TLS + capability + audit | M13 §1.2.1 AuthMiddleware |
| inv_M13_02 | 单机优先——不引入 Kafka/Redis/RabbitMQ/K8s | 架构硬约束审计 |
| inv_M13_03 | Worker pool 严格隔离——intent_handler/eval/ingest/background/cron 独立 pool | M13 §2.0 ResourceGovernor |
| inv_M13_04 | HITL 审批超时触发 timeout policy——kill_pause（默认）/ auto_deny / auto_approve | M13 §2.4 ApprovalRequest |
| inv_M13_05 | ResourceGovernor 与 M3 OSMemoryGuard 共享三级降级阈值——任一触发即执行 | M13 §2.0 三级降级 |
| inv_M13_06 | 所有出站请求经 EgressGateway → M11 SafeDialer.DialContext 完整五阶段 SSRF 防护 | M13 §1.2.2 EgressGateway |

---

## 1. 对外接口

### 1.1 CLI

```
polaris query "..." / chat / serve / config get|set <k> <v> /
        config history / config revert <version> / config diff <v1> <v2> /
        cron list|add|remove / sessions list|switch|resume <id> / status / doctor /
        export [--output polaris-backup-YYYYMMDD.jsonl] / import <backup.jsonl> /
        tool quarantine <toolID> /
        migrate openclaw [--dry-run] [--with-memory] [--smart] [--stage] /
        memory process-staging
```

AgentREPL: 逐行读 stdin，"/" 前缀→内置命令（/help /sessions /switch /skills /memory /status /quit），否则→ StreamInfer，EventToken→stdout。

配置版本控制: 用户配置每次变更原子记录到 events 表（source_type='user_config_change'），享受 EventLog 完整审计 + 回滚能力。`polaris config history` 显示变更历史；`polaris config revert <version>` 回退；`polaris config diff <v1> <v2>` 对比差异。

数据导出/迁移: `polaris export [outfile.jsonl]` 流式导出 `chat_sessions`、`chat_messages` 和 `kv_store` (config 前缀)。`polaris import <infile.jsonl>` 幂等 upsert 恢复。

**外部平台迁移**: `polaris migrate openclaw`（`cmd/polaris/migrate_openclaw.go`）从 ~/.openclaw 导入配置/密钥/记忆/人设:
- 配置与密钥 → `configs/defaults.toml`；SOUL.md/AGENTS.md → 系统 prompt + M5 记忆层
- 记忆 SQLite → EventLog（`--with-memory`）；`--stage`（默认）写隔离命名空间 `salience=0.3`，`--smart` 启用 LLM 预压缩
- 迁移后 `polaris memory process-staging` 触发去重→Salience重算→提升主线
- 技能 SKILL.md 仅拷贝源码并标注"需人工编译为 Wasm"

月度成本报告：cron `0 0 1 * *` → 生成 `monthly_cost_report.md`，含 by_provider / by_task_type / by_session / by_call_type 四维度。实现见 `pkg/edge/scheduler/cost_report.go`：查询 `events` 表上月 `inference.*` 事件，从 payload 提取 input_tokens + output_tokens，按内置 provider 成本系数（deepseek ¥0.27/1M、anthropic $3/1M 等）计算实际费用并聚合。DB 不可用时降级为空报告。`polaris config budget set <amount>` 配置 monthly_budget，写入 `kv_store`（键 `config:budget:monthly_usd`）。

### 1.2 HTTP REST API

完整路由见 `pkg/gateway/server/server.go`。以下按业务域分组。

```
─── 系统 ───────────────────────────────────────────────
GET  /healthz                              健康检查
GET  /readyz                               就绪检查
GET  /v1/status                            系统状态（Agent 状态 + Token 统计 + 内存）
GET  /v1/doctor                            诊断报告
GET  /metrics                              Prometheus 指标

─── Agent 对话 ─────────────────────────────────────────
POST /v1/agent/query                       同步查询
POST /v1/agent/stream                      SSE 流式推送（text/event-stream）
POST /v1/agent/{taskID}/interrupt          [UserInterrupt] 中断（详见 §1.2.5，<200ms SLO）
GET  /v1/logs/stream                       实时日志 SSE（EventSource GET）

─── 会话管理 ───────────────────────────────────────────
GET    /v1/sessions                        列出会话
GET    /v1/sessions/{id}                   会话详情
DELETE /v1/sessions/{id}                   删除会话
GET    /v1/sessions/{id}/recap             会话摘要
GET    /v1/sessions/{id}/context           上下文诊断（token 用量 + 压缩统计，见 §1.2.6）

─── 搜索与洞察 ─────────────────────────────────────────
GET  /v1/search                            全文搜索
GET  /v1/insights                          系统洞察报告

─── Provider 与模型 ────────────────────────────────────
GET    /v1/providers                       列出 Provider
POST   /v1/providers                       创建 Provider
PUT    /v1/providers/{providerID}          更新 Provider
DELETE /v1/providers/{providerID}          删除 Provider
POST   /v1/providers/{providerID}/test     测试 Provider 连通性
GET    /v1/providers/{providerID}/models   列出模型
POST   /v1/providers/{providerID}/models   添加模型
PUT    /v1/providers/{providerID}/models/{modelID}    更新模型
DELETE /v1/providers/{providerID}/models/{modelID}    删除模型

─── 配置 ───────────────────────────────────────────────
GET  /v1/config                            读取运行配置
GET  /v1/config/model-roles               读取模型角色映射
PUT  /v1/config/model-roles               更新模型角色映射

─── 工具与技能 ─────────────────────────────────────────
GET  /v1/tools                             列出已注册工具
GET  /v1/tools/schemas                     工具 JSON Schema
GET  /v1/skills                            列出已安装技能
POST /v1/tools/{name}/execute              直接执行工具
POST /v1/skills/install                    安装技能（接受 Wasm 载荷或源码）

─── MCP Server ─────────────────────────────────────────
GET    /v1/mcp-servers                     列出 MCP Server
POST   /v1/mcp-servers                     注册 MCP Server
PUT    /v1/mcp-servers/{serverID}          更新 MCP Server
DELETE /v1/mcp-servers/{serverID}          删除 MCP Server
POST   /v1/mcp-servers/{serverID}/test     测试 MCP Server 连通性

─── 插件与市场 (Marketplace) ──────────────────────────────
GET    /v1/plugins/catalog                 读取聚合市场目录缓存（MCP/Skill/Plugin/App）
POST   /v1/plugins/sync                    异步拉取并解析远程市场 Manifest
GET    /v1/plugins/marketplaces            获取已订阅市场列表
POST   /v1/plugins/marketplaces            添加订阅远程市场
DELETE /v1/plugins/marketplaces/{id}       删除订阅市场
POST   /v1/plugins/install                 从 catalog 安装并使能目录项（写 ToolRegistry）
DELETE /v1/plugins/{catalogID}             卸载已安装目录项
POST   /v1/mcp/create                      直接创建自定义 MCP Server 记录
POST   /v1/skills/create                   直接创建自定义 Skill 记录
POST   /v1/plugins/create                  直接创建自定义 Plugin 记录
POST   /v1/apps/create                     直接创建自定义 App 记录

─── 第三方接入（Channel）────────────────────────────────────
GET    /v1/channels                        列出接入
POST   /v1/channels                        创建接入
PUT    /v1/channels/{channelID}            更新接入
DELETE /v1/channels/{channelID}            删除接入
POST   /v1/webhooks/{channelType}/{channelID}   Webhook 入站接收

─── 自动化（Automation）────────────────────────────────
GET    /v1/automations                     列出自动化任务（含执行状态）
POST   /v1/automations                     创建自动化任务
PUT    /v1/automations/{id}               更新（patch 语义）
DELETE /v1/automations/{id}               删除（含执行历史）
GET    /v1/automations/{id}/runs          执行历史（limit 默认 20）
POST   /v1/automations/{id}/trigger       手动立即触发

─── HITL 审批 ──────────────────────────────────────────
GET  /v1/approvals/pending                 [ESCALATE] 待审批列表
POST /v1/approvals/{id}/resolve            [ESCALATE] 审批决定（approve/deny）

─── 偏好与电脑操控 ─────────────────────────────────────
GET  /v1/preferences                       读取全量偏好（KV 表）
PUT  /v1/preferences/{key}                设置单项偏好（含 computer_use.*、permission_mode）

─── 预算 ───────────────────────────────────────────────
GET  /v1/config/budget                     读取月度预算
PUT  /v1/config/budget                     设置月度预算

─── 评测 ───────────────────────────────────────────────
POST /v1/eval/run                          触发 Eval 运行

─── 数据导出/导入 ──────────────────────────────────────
GET  /v1/export/trajectories               导出轨迹数据
GET  /v1/export/backup                     导出全量备份（JSONL）
POST /v1/import/backup                     恢复备份（幂等 upsert）

─── OpenAI 兼容 ───────────────────────────────
POST /v1/chat/completions                  OpenAI 兼容端点（第三方客户端接入）
```

SSE 请求 (`POST /v1/agent/stream`): query(string,req) / session_id(string,opt) / model(string,opt) / temperature(float32,opt) / max_tokens(int,opt)
SSE 事件 (text/event-stream): "token" | "tool_call" | "tool_result" | "thinking" | "error" | "complete" | "context_warning" | "status/compacting" | "status/compacted" → data: <JSON>\n\n
- `context_warning`：上下文使用率 ≥ warnPct 时触发；payload: `{usage_pct, thrashing}`（thrashing=true 时前端变红色 Banner 并隐藏"立即压缩"按钮，见 §1.2.6）
- `status/compacting`：压缩进行中（前端显示蓝色 Indicator）
- `status/compacted`：压缩完成（前端在末尾消息标记分隔线）

#### 1.2.1 认证

AuthMiddleware:
  1. X-Session-Token → 匹配本地 Bearer Token (~/.polarisagi/polaris/.session_token 0600) → 放行; loopback 不免密 → 401
  2. X-API-Key → [CredentialVault] KeychainProvider.Verify SHA-256 常量时间比较 → 失败 401
  公网: JWT Ed25519 + TLS 1.3; 3 次失败 → IP 冷却 5min

#### 1.2.2 Egress Gateway

HTTP 层出站适配器，委托 M11 SafeDialer（M11 §6 统一安全 Dialer）执行完整 SSRF 防护。本层仅维护 Provider 域名白名单作为预检（api.deepseek.com, api.anthropic.com, api.openai.com, api.github.com, localhost）——不在白名单的域名提前拒绝，减少 SafeDialer DNS 查询开销。实际连接（DNS 解析、CIDR 校验、TOCTOU 消除、IP 锁定）全部由 M11 SafeDialer.DialContext 统一执行。扩展: /config network allow example.com:443（追加白名单，仍需经 SafeDialer 完整校验）。

#### 1.2.3 Sealed 状态

`/v1/status` 响应中的 `sealed` 字段由 `observability.GlobalKillswitchStage` 原子量驱动（`>= KillFullStop(3)` 时为 `true`）。KillSwitch 状态变迁通过 `StateChangeCallback` 同步写入该原子量。

KillFullStop 触发后，系统写入 `dataDir/.fullstop` 文件，下次重启时 `main.go` 检测到该文件并拒绝启动（见 M11 §4.2）。

#### 1.2.4 优雅关闭

`Server.Shutdown(ctx)` 执行顺序：
1. `cronCancel()` — 停止 Cron runner goroutine（拒绝新触发，已运行的 automation goroutine 持有自身超时 ctx 自然结束）
2. `channelMgr.StopAll()` — 停止所有聊天平台 poller
3. `srv.Shutdown(ctx)` — 等待进行中的 HTTP 请求排空，超时 30s

`main.go` 在 Shutdown 完成后等待 DatabaseWriter flush 残余批次（dbWriterDone channel），确保 AI 核心数据不丢失。

#### 1.2.5 `[UserInterrupt]` 端点（inv_global_08，<200ms）

`POST /v1/agent/{taskID}/interrupt`，Body：`{ action: "resume" | "redirect" | "abort", instruction?: string }`。

**协议**：Auth(X-Session-Token) 验证后，通过 MutationBus 写入 pending 标记并向 EventLog 推送 `agent_interrupt_requested` 事件，触发 M4.ContextCancel() 取消所有子 goroutine，立即返回 202（不等待 S_INTERRUPT 确认）。跨 session 中断需持有 `interrupt_remote` Capability。

**SLO**: 端点接收 → ContextCancel 完成 < 200ms（M3 `polaris_user_interrupt_latency_ms` Histogram 监控）

**action 语义**:
- `resume`: instruction 注入 ZoneImmutable（[TaintLevel]=TaintUserReviewed, source='user_interrupt'），Agent 恢复原状态继续
- `redirect`: 跳转 S_PLAN 重新规划，不消耗 ReplanCount
- `abort`: 进入 S_FAILED + Saga 逆序补偿 + workspace GC

**约束**:
- 与 [KillSwitch] 同等优先级但作用域为单 task；KillSwitch FULLSTOP 覆盖所有 task，UserInterrupt 仅当前 taskID
- 同 task 30s 内重复中断 → HTTP 429（防抖）
- task 不存在 / 已 S_COMPLETE/S_FAILED → HTTP 404

#### 1.2.6 上下文压缩机制（Compressor）

`pkg/gateway/server/compressor.go` — 对齐 Claude Code 自动压缩机制。

**阈值模型**（百分比，非绝对 token 数）：

| 参数 | 默认值 | 含义 |
|------|--------|------|
| `context_window` | 32768 | 上下文窗口大小（token） |
| `auto_compact_pct` | 95% | 自动压缩触发阈值（`contextWindow × pct`） |
| `warn_pct` | 80% | 警告 Banner 触发阈值 |
| `max_thrash_count` | 3 | 连续 thrashing 上限 |

TOML 配置：`configs/defaults.toml [compressor]`。

**Thrashing 防抖**：`compact()` 追踪 `thrashedCount`——压缩后仍超阈值则累计；连续 ≥ `max_thrash_count` 次后停止自动压缩，SSE 推送红色 `context_warning{thrashing:true}`，等待用户手动干预（`ForceCompact()` 重置计数给一次新机会）。

**调用链**：`sse.go` 每轮推理前调用 `Compressor.NeedsCompact()` → thrashing 状态下跳过 → 否则调 `Compressor.Compact()`（LLM 摘要） → 压缩前后各推一次 SSE `status/compacting` / `status/compacted`。

`GET /v1/sessions/{id}/context` 返回当前 `ContextStats`（token 数 / 阈值 / 使用率 / 上次压缩时间 / thrashing 状态），供前端 `/context` 斜线命令展示。

#### 1.2.7 斜线命令系统（SlashCommandRouter）

`pkg/gateway/server/slash_commands.go` — 在 SSE 层拦截用户输入（provider 选取后、LLM 推理前），不消耗 TokenBudget，不进入 Agent FSM。

| 命令 | 行为 |
|------|------|
| `/context` | 读取 `ContextStats` → SSE 输出 token 使用率统计表格 |
| `/compact` | 调 `ForceCompact()`（跳过阈值检查 + 重置 thrashing 计数）|
| `/clear` | 物理删除 DB 中非 system 消息 + 前端 `clearView()` 双清 |
| `/help` | SSE 输出所有注册命令列表 |

`SlashCommandRouter.Dispatch()` 返回 `CommandResult{Handled, Response, UpdatedHistory}`；Handled=true 时调用方短路，直接发 `complete` 事件。命令回复作为助手消息持久化到 DB。

### 1.3 WebSocket [计划：可选升级路径]

> 当前推送走 SSE，client→server 走 REST。WebSocket 升级设计约束（实现时以此为规范）：
- 读 goroutine: ClientMessage JSON→Intent→Agent.Input
- 写队列 cap=256：Critical 事件（tool_call/error/approval/complete）不可丢弃；Streaming（token/thinking）可合并
- Critical 满→合并 Streaming 腾 slot；无可合并→Force Disconnect + [EventLog] 回放；严禁 drop-oldest
- `polaris_ws_coalesced_events` Counter 监控

### 1.4 Web UI

> 实现规约已迁出为独立文档，见 §8 / M13-Interface-WebUI.md。

ServeWebUI:
- `DEV_MODE=1` → 反向代理 Vite dev server (`:5173`)
- 生产 → `http.FileServer(http.FS(subFS))` 挂载 `go:embed all:dist`

FeatureGate: `FeatureWebUI` 控制是否注册 `/` 路由。关闭时仅 REST API 可用（API-only 模式），不影响 CLI 功能。见 M03 §5。

### 1.5 Rate Limiting

RateLimiterMiddleware:
  Token Bucket GCRA, 双层隔离 (进程指纹+client_type 复合键)
  fingerprint: 本地→PID+启动时间 hash; 远程→M11 Ed25519 AgentIdentity 公钥 hash
  熔断: 连续 3 个 1s 窗口>100%配额→隔离 30s (429+Retry-After:30)
  配额(per fingerprint+client_type): CLI 50/s; WebUI 30/s; A2A 30/s; /_admin/ 10/s
  响应: HTTP 429+Retry-After

---

## 2. 对内调度

### 2.0 Resource Governor

ResourceGovernor (`pkg/edge/scheduler/scheduler.go`，**阈值 SSoT: `spec/state.yaml §thresholds.memory_pressure`**，与 M3 OSMemoryGuard 共享同一配置节，禁止独立硬编码):
  字段: maxConcurrent / inFlight(atomic.Int32) / cpuThreshold(70%) / memThresholdMB(1024MB=L2 紧急快速拒绝门限，[Tier-0-Limit])

  Admit(priority, estimatedCostMB):
    1. priority=0→始终放行（**已修复**：原先 priority=0 可无限制入队导致 OOM，现加 `maxConcurrent×4` 硬上限）
    2. CPU>70% 或空闲<1024MB→拒绝非用户交互
    3. >50MB 双层校验: OSMemoryGuard.CurrentPressureLevel(30s滑窗) + 瞬时FreeMemory; Normal且瞬时>512MB→放行; Elevated→仅priority<=1; Critical→拒绝所有非用户交互
    4. inFlight>=maxConcurrent→优先级降序抢占
  Release: inFlight--; 空闲>memThresholdMB+256MB 回滞→Cond.Broadcast()

  降级(三级，与 M3 共享阈值):
    L1 (预警): 空闲 <1.5GB 或 CPU >70% → 拒绝 priority>=3
    L2 (紧急): 空闲 <1.0GB 或 CPU >85% → 拒绝 priority>=2
    L3 (临界): 空闲 <512MB 或 OOM → 拒绝 priority>=1 + 通知 M3

  **local_only 死锁恢复**: `local_only` 下 OSMemoryGuard 卸载本地 LLM → 任务堆积占内存 → 无法重载 LLM（循环死锁）。当 (a)ErrLocalModelUnavailable>30s + (b)压力>=L2 + (c)待处理>0 时，通知 M8 将 Priority>=2 任务回退 Suspended(oom_evicted)；每释放 256MB 重试重载；仍无法重载 → HITL 手动介入

### 2.1 任务队列与全局并发信号量

TaskStore: Enqueue/Dequeue/MarkComplete/MarkFailed/ListPending/Close
  实现: SurrealDBTaskStore(key:task:{id}) / SQLiteTaskStore; [Tier-0-Limit] 非热路径
  类型定义: `pkg/edge/scheduler/scheduler.go`（ResourceGovernor / TaskQueue）、`pkg/edge/hitl/gateway.go`（HITLGateway）、`pkg/edge/scheduler.go`（TrafficSplitter，该文件余量已 Deprecated，迁移至 `pkg/edge/scheduler/` 中）

**Global Semaphore (并发限制)**:
在 M13 引入全局跨模块并发控制信号量（Global Semaphore）。确保 LLM 推理节点（M1/M4/M9 均会调用 LLM）的**总并发度**上限不超出硬件或 API 的物理承载极限。当多个 worker (Agent, BackgroundTask, Cron 等) 争抢 LLM 资源时，通过 M13 Global Semaphore 进行排队，防止过载打爆本地 GPU/内存 或触发 API 供应商 429。默认配置 Tier 0 仅允许 GlobalSemaphore=1，Tier 1+ 视配置（`config.GlobalConcurrency`）决定。

TaskQueue 交付语义: at-least-once, 幂等键 = ScheduledTask.ID。

### 2.2 定时任务

Scheduler.Start:
  每1h: memorySystem.RunConsolidation
  每天 02:00: evalRunner.RunNightly
  每5min: killSwitch.CheckAndAct([TokenBurnRate])
  每周日 03:00: memorySystem.CheckEmbeddingDrift
  每30min(空闲): selfImprove.IdleLoop

### 2.3 Resource Reaper

ResourceReaper:
  组成: storageFabric, skillLibrary, memorySystem; minInterval(24h)
  Reap(前置 isDeepIdle();超6:00未获→M9 SuspendAll→清理→ResumeAll→M11 Audit):
    1. PruneDeprecatedSkills: 30天未检索+成功率<30%→删除 Wasm
    2. PruneOrphanEntities: 无入/出边+90天未更新
    3. PruneWorkspaceFiles: >7天且关联 Task Status∈{Done,Failed}→`os.RemoveAll(workspace/<task_id>/)`; 按 CreatedAt 升序回收至 < maxSize×0.7; 紧急模式(写入拦截触发)→同步 RunNow，跳过定时等待
    4. CompressColdArchive: >180天 JSONL→zstd
    5. storageFabric.Vacuum
    6. storageFabric.CompactSurrealDB
    7. storageFabric.RebuildStaleIndexes
  注册: "0 4 * * *"

### 2.4 HITL ([ESCALATE])

HITLGateway: pending(map[string]*ApprovalRequest)/store(*HITLStore)/notifier(Notifier)/killSwitch([KillSwitch])/auditLog

Notifier: NotifyApproval(req)/NotifyResolved(req,outcome)
  实现: SlackNotifier(Webhook)/EmailNotifier(SMTP)
  重试: 3次(100ms→500ms→2s);失败→回退 Slack→Email;Email→本地(chat:stderr+BEL;serve:syslog CRITICAL+Web UI /_admin/alerts)
  上限: min(HITL timeout×10%,2min);确定性失败不重试

HITLStore: Persist(INSERT OR REPLACE)/UpdateStatus(UPDATE status/comment/resolved_at)/LoadPending(SELECT WHERE status='pending')

ApprovalRequest:
  ID/AgentID/Action(string)/Detail(string)/RiskLevel(string)/CreatedAt/Timeout（默认值见 `spec/state.yaml §m13_scheduler.hitl_default_deadline_minutes_normal`，紧急/长程档见同节 _urgent / _long，Per-TaskType 可覆盖）
  TimeoutPolicy: kill_pause(默认)/auto_deny/auto_approve(policy_version>=2+管理员授权)
  Status: pending/approved/denied/timeout

  auto_approve 硬编码约束(internal/config/immutable_constants.go,CI 不可变内核):
    禁止: write_network, privileged, delete_data, execute_system, modify_policy
    白名单: read_local_file, log_rotate, cache_evict, stats_collect
    [Taint-Medium]感知: ActiveContext.TaintLevel>=[Taint-Medium]→auto_approve 失效,升级 HITL
    敏感路径 glob: **/.env*, **/*id_rsa*, **/*.pem, **/credentials*, **/*.key, **/secret*, **/.ssh/**, **/.aws/**, **/.gcloud/**, **/kubeconfig*
    Symlink 防御: filepath.EvalSymlinks(filepath.Abs(filepath.Clean(path))) 先于 glob
    原子 etag 校验: auto_approve 放行决策到 JIT Token 签发为临界区——签发前原子比对当前 [Cedar-Gate] policy_version/etag 与决策时刻记录的 decision_etag。etag 不一致 → 决策上下文已过期，拒绝 auto_approve，操作升级为 HITL 审批。此校验防止 auto_approve 放行后数毫秒内 Cedar 策略热更新导致 Token 刚签发即被 M7 L3PolicyMonitor 掐断（M7 §4.7），避免产生难以归因的 `l3_policy_revoked_network_killed` 审计震荡

RequestApproval:
  1. 加入 pending 2. 持久化 SQLite 3. Notifier 发送 4. [EventLog] Subscribe ApprovalResolved
  5. 等待: 收到→nil/ErrApprovalDenied; 超时→用户交互:ErrApprovalTimeout+S_ROLLBACK(不触发[KillSwitch]); 后台:auto_deny

ResolveApproval:
  1. pending 查找→Approved/Denied+Comment 2. 持久化+审计 3. [MutationBus]→[EventLog]

ReloadOnStartup: 加载 pending→超时检查→超时则 ApprovalTimeout；未超时→重启计时器+重通知（dedup:SHA-256(requestID+retry_seq)，>50条→仅最近50+M3 CRITICAL）

### 2.5 TrafficSplitter

AgentVersion: Version(string)/Type(baseline/candidate/shadow/rollback)/ConfigRef/PromptSetRef/SkillSnapshotID/ModelID/CreatedAt/EvalResults(*EvalRunReport,opt)

TrafficSplitter: baseline(*AgentVersion)/candidate(*AgentVersion)/percent(atomic.Int32)
  Route: percent<=0→baseline;>=100→candidate;else fnv32a(SessionID)%100 < percent → candidate
    注: 使用 fnv32a 确保同一 SessionID 内所有请求路由一致（默认 Go map hash 不可轾入）
  SetPercent: atomic.Store; Rollback: atomic.Store 0+告警
  分工: M9 决策+回滚检测; M13 执行; M12 对比

---

## 3. MCP

StartMCPServer: 1.创建 Server(name="polaris-agent",v0.1.0) 2.注册 execute_skill/search_knowledge(InputSchema) 3.注册 memory://episodic/recent 4.StdioTransport

MCPServerConfig: Name/Command/Args([]string)/Env(map[string]string)/AutoConnect(bool,true)/Timeout(30s)
MCPConfig: Servers([]MCPServerConfig)

ConnectMCP: 1.遍历 Servers 2.CommandTransport exec.Command→MCP 会话 3.session.ListTools→Tool{Name,Source=SourceMCP,SourceURI=server.Name}→toolRegistry

---

## 6. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| HTTP server 阻塞/超载 | 限流 (429+Retry-After) + 504 超时 | 负载降低后恢复 |
| SSE 连接断开 | 客户端 EventSource 自动重连（指数退避） | — |
| TaskQueue SurrealDB-Core 持久化失败 | 降级纯内存队列 (at-most-once) + CRITICAL 告警 | SurrealDB-Core 恢复后切回 at-least-once |
| Cron 定时器错失 | gocron 自动补偿 (错失→立即执行一次) | — |
| HITL Notifier 全部通道失败 | 本地回退 (chat:stderr+BEL / serve:syslog CRITICAL) | — |
| ResourceGovernor 拒绝新任务 (L3 临界) | 503 + Retry-After | 内存恢复后 Admit 放行 |

与 OSMemoryGuard 协同: ResourceGovernor 实时读取 OSMemoryGuard.CurrentPressureLevel() → Admit 准入。shared semantics: L1/L2/L3 阈值同源(见 00-Global-Dictionary §1-ter XR-07)。

## 6-bis. 已知 Bug 修复记录

| # | 级别 | 文件 | 函数 | 问题描述 | commit |
|---|------|------|------|----------|--------|
| 1 | P3 | pkg/gateway/server/sessions.go | handleGetSession | `TaskDuration` 时间解析 layout 使用 `"2006-01-02T15:04:05Z"`，SQLite `datetime()` 实际输出 `"2006-01-02 15:04:05"`（空格分隔无 Z），两次 `time.Parse` 静默失败导致 `TaskDuration` 永远为 0。改用兼容两种格式的 `parseDBTime` 辅助函数。 | — |
| 2 | P1 | pkg/gateway/server/cron.go | executeAutomation | `result_action="channel:XXX"` 路径调用 `channelMgr.SendReply(channelType="", ...)` 走 `default` 分支静默丢弃，自动化频道回复完全失效。修复：先从 `channels` 表查 `type` + `config_json`，传正确 `channelType` 再调用 `SendReply`。 | — |
| 3 | P3 | pkg/edge/scheduler/cost_report.go | generateCostReport | `period` 字符串用 `fmt.Sprintf("%d-%02d", now.Year(), int(now.Month())-1)`，1月时 `Month()-1=0` 输出 `"YYYY-00"`。改为从 `monthStart.Year()` + `monthStart.Month()` 派生，1月正确输出上年12月 `"YYYY-12"`。 | — |

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m13_scheduler`。

## 7. 跨模块依赖与契约

| 关联模块 | 关键契约 | 位置 |
|---------|---------|------|
| M1 Inference | Provider 路由（API 调用 → SafeDialer 出口）| M1 §4 |
| M2 Storage | EventLog 持久化、MutationBus 串行写 | M2 §2.1, §2.3 |
| M3 Observability | OSMemoryGuard 三级降级共享阈值、ResourceGovernor 联合决策 | M3 §6, M13 §2.0 |
| M4 Agent Kernel | Agent Intent 输入（CLI/HTTP → Agent.Input）| M4 §2 |
| M8 Orchestrator | HITL 挂起/恢复、TrafficSplitter 流量分发 | M8 §1.5, M13 §2.5 |
| M9 Self-Improve | ProgressiveRollout 执行分发、ResourceReaper 闲时清理 | M9 §2.3, M13 §2.3 |
| M11 Policy Safety | KillSwitch FullStop → SealedMiddleware 503、SafeDialer 网络出口 | M11 §4, §6 |
| 接口定义 | SafeDialer/Blackboard/TaskEntry/ScheduledTask | internal/protocol/interfaces.go, types.go |
| 全局字典 | ESCALATE/KillSwitch/SSRFGuard 定义 | 00-Global-Dictionary §3, §4 |
| 时序图 | KillSwitch 触发链（M13 SealedMiddleware 503 响应）| DIAGRAMS.md#killswitch |

---

## 8. Web UI 规约

> 前端实现的单一权威源。栈: Alpine.js + Tailwind CSS v4 + DaisyUI + Vite 6 + go:embed + marked | [FeatureGate.FeatureWebUI] | [Tier-0-Limit]

### 8.1 目录结构

```
web/
├── embed.go                  # go:embed all:dist → WebUIFS embed.FS
├── vite-plugin-fragments.js  # 构建时展开 <page-fragment src="…"> 为内联 HTML
├── src/
│   ├── index.html            # 壳页（侧边栏 + 10个page-fragment + 浮动日志抽屉）
│   ├── pages/                # 页面 HTML 片段
│   │   ├── chat.html         # 新对话（含权限模式下拉，绑定 $store.computer）
│   │   ├── sessions.html     # 会话历史（独立）
│   │   ├── search.html       # 全文搜索（独立）
│   │   ├── skills.html       # 已安装技能列表（独立）
│   │   ├── plugins.html      # 插件聚合大市场（插件/应用/MCP/技能/市场）
│   │   ├── automation.html   # 聚合Tab：日常定时任务 · 工作流 · 触发器 · 待办审批
│   │   ├── monitor.html      # 聚合Tab：状态 · 洞察 · Agent
│   │   ├── settings.html     # 聚合Tab：提供方 · 接入 · 配置 · 电脑操控
│   │   ├── eval.html         # 评测套件（独立）
│   │   └── onboard.html      # 首次引导向导（OnboardWizard：Provider/Model/Channel 三步配置，DOMContentLoaded 后 400ms 延迟检查）
│   ├── js/
│   │   ├── app.js            # Alpine stores 入口 + marked 配置 + URL路由初始化
│   │   ├── sse.js            # SSEClient（fetch+ReadableStream + 指数退避重连）
│   │   ├── utils.js          # 文本过滤与通用工具
│   │   ├── i18n.js           # 中/英 i18n 数据
│   │   └── store/            # 按域拆分的 Alpine 状态管理
│   │       ├── chat.js       # 对话状态机
│   │       ├── logs.js       # 日志 SSE + 抽屉状态
│   │       ├── nav.js        # 页面路由 + 侧效触发
│   │       ├── statusBar.js  # 顶栏轮询
│   │       └── …（approvals/sessions/skills/plugins/providers/channels/config/
│   │              agents/insights/cron/eval/search/onboard/toast/i18n/modelRoles/computer）
│   └── css/style.css         # Tailwind v4 入口（@import "tailwindcss"）+ 主题色 token
├── package.json              # 生产依赖: alpinejs + marked；开发: @tailwindcss/vite + daisyui + vite
└── vite.config.js            # Vite 构建配置
dist/                         # Vite 输出（gitignore；make build-ui 生成）
```

---

### 8.2 页面结构与导航

**导航架构**（已实施）— 侧边栏 10 个入口，3 个聚合页通过 Tab 内嵌，日志独立为浮动抽屉：

| 导航入口 | 路由 | 子 Tab / 说明 | 主要 API |
|----------|------|--------------|---------|
| 新对话 | `/` | 权限模式下拉内联于输入区 | `POST /v1/agent/stream` (SSE) |
| 搜索 | `/search` | — | `GET /v1/search` 提交触发 |
| 会话 | `/sessions` | — | `GET /v1/sessions` |
| 插件 | `/plugins` | 聚合Tab：插件 · 应用 · MCP · 技能 · 市场（五合一视图） | `/v1/plugins/catalog` · `/v1/plugins/sync` · `/v1/plugins/install` |
| 自动化 | `/automation` | 聚合Tab：定时自动化 · **工作流** · 触发器(Webhook) · 人工待办审批(HITL) | `/v1/automations` · `/v1/workflows` · `/v1/approvals/pending` · `/v1/approvals/{id}/resolve` |
| 监控 | `/monitor` | Tab: 状态 · 洞察 · Agent | `/v1/status` 轮询 · `/v1/insights` · agent_state/agent_config |
| 设置 | `/settings` | Tab: 提供方 · 接入 · 配置 · 电脑操控 | `/v1/providers` · `/v1/channels` · `/v1/config` · `/v1/preferences` |
| 评测 | `/eval` | — | 提交触发 |
| **日志抽屉** | （FAB 唤出，不占导航位） | 浮动侧滑面板 | `GET /v1/logs/stream` (EventSource) |

**旧路由兼容映射**

`app.js` 的 `legacyPageMap` 在 DOMContentLoaded 时将旧 URL 重定向至新聚合页，无需服务端 redirect：

`app.js` 的 `legacyPageMap` 在 DOMContentLoaded 时将旧 URL（status/insights/providers/channels 等）重定向至新聚合页，无需服务端 redirect。

**Tab 懒加载**：聚合页各子 Tab 首次激活才加载数据，页级 `x-data` boolean flag 去重（`inv_webui_07`）。例外：Providers/ModelRoles 进入 settings 时预加载（首屏需要）。

---

### 8.3 核心协议与渲染

> UI 硬规则集中在 §8.5 `inv_webui_NN` 表（单一 grep 源）；下方 `(inv_*)` 为交叉引用。

#### 8.3.1 Chat SSE 渲染

- **流端点**: `POST /v1/agent/stream` (`text/event-stream`)。因需 POST body，使用 `fetch+ReadableStream`（`sse.js:SSEClient`），非原生 `EventSource`。
- **状态机** `(inv_webui_08)`: `IDLE → SUBMITTING → THINKING → STREAMING → TOOL_RUNNING → STREAMING → COMPLETE → IDLE`。
- **工具生命周期推送**: 在 `TOOL_RUNNING` 期间，要求 SSE 额外推送细粒度事件 (`tool_start`, `tool_progress`, `tool_success`, `tool_error`)，支持中间态渲染。
- **异常退避**: 1s→2s→4s→8s→16s→30s，超 10 次转 ERROR。
- **中断恢复**: 接收 `error(interrupted)`，保留 `currentTokens` 并打橙色"⚠ 已中断"徽章，消息持久化入列。
- **幂等去重** `(inv_webui_10)`: `dedupeRunID(sessionID, input)` 在 5s 窗口内对相同 (sessionID, input) 返回同一 runID，防止重复提交。

#### 8.3.2 文本规范化与 Markdown

- **安全沙箱** `(inv_webui_09)`: `marked` 渲染，白名单过滤 HTML（清 `script`, `on*`, `javascript:`），所有 `<a>` 强制 `rel="noopener noreferrer"`，确保 XSS 安全。
- **静默过滤**: `sanitizeContent()` 剥除 XML (`<tool_call>`), `[[reply_to_*]]`, `NO_REPLY`。
- **思维剥离**: `event:thinking` 仅入 `ThinkingPanel`，不混入用户消息气泡。

#### 8.3.3 日志 SSE 流（浮动抽屉模式）

- 使用原生 `EventSource`（GET，无 POST body 需求）连接 `GET /v1/logs/stream`。
- 连接生命周期绑定抽屉开关 `(inv_webui_06)`：`openDrawer()` 触发 `connect()`，`closeDrawer()` 触发 `disconnect()`，**避免后台常驻 SSE 连接**。
- 环形缓冲 1000 条，满则截断至 500 条。
- 3s 固定间隔断线重连（无退避，低频日志容忍延迟）。
- 级别过滤通过 `levelFilter` 参数重新建立连接（`setLevel()` → `connect()`）。

---

### 8.4 UI 组件与交互

| 组件/交互 | 触发/机制/结果 |
|----------|--------------|
| **ToolExecutionAccordion** (工具执行折叠面板) | **渲染机制**: 顶部固定显示工具执行总耗时（基于底层 `created_at` 和 `updated_at` 差值，对标 Codex 体验）。<br>**状态**: 折叠态（如 "正在使用 Browser 搜索..."）；展开态（内嵌 Markdown/Terminal，不仅实时回显输出，还必须**高亮显示实际执行的具体命令**）；<br>**文件内容与 Diff (对标 Codex)**：针对创建或修改文件的操作，UI 需支持点击展开查看完整文件内容及 Diff 差异对比；<br>结束态（成功绿勾 / 失败红叉+重试按钮）。 |
| **StatusBar** | 10s 轮询 `/v1/status`。Token 烧率 >80% 出橙色徽章，>95% 红色告警并提醒将压缩。 |
| **ApprovalCard** | 风险分级色阶（蓝/橙/红），含闪烁倒计时。任务导航项显示待审数量角标。 |
| **CompactionDivider** | 遇压缩检查点 `at_message_id` 注入占位块，支持从此分支发起新会话。 |
| **OnboardWizard** | 首次引导（DOMContentLoaded 后 400ms 延迟检查）。配置 Provider/Model/Channel 三步流程。 |
| **浮动日志抽屉** | 右下角 FAB 按钮唤出，CSS `transform: translateX` 侧滑动画。打开时建立 EventSource，关闭时断开 `(inv_webui_06)`。未读日志时 FAB 显示绿点。 |
| **Tab 内嵌导航** | Monitor/Settings/Tasks/Capabilities 四页内嵌 `.tab-bar`。Tab 面板用 `x-show`（非 `x-if`）保留 DOM 状态 `(inv_webui_11)`，避免 `setInterval` 在切换时重置。 |
| **键盘快捷键** | `Enter` 提交，`Shift+Enter` 换行，`Ctrl+C` 中断流，`/` 唤出斜杠补全。 |
| **主题切换** | `--color-surface` 变量系（Tailwind `@theme`）。支持 system/dark/light/terminal，持久化至 localStorage。 |
| **语言切换** | `$store.i18n.setLang('zh'|'en')`，i18n 数据集中在 `js/i18n.js`，涵盖全量 UI key。 |

---

### 8.5 构建与不变量

**构建/运行**:
- 生产: `make build-ui` → `npm install && npm run build` → 输出至 `web/dist/`。
- 开发: `make dev-ui` → Vite dev server `:5173`，代理 `/v1` 至 `:28888`。
- `make build` 自动先调 `make build-ui`（Rust FFI → Web UI → Go binary）。

**[Tier-0] 核心不变量** — UI 硬规则单一 grep 源；`(inv_webui_NN)` 在 §8.3/§8.4 处可交叉引用。

| ID | 约束 | 验证位置 |
|----|------|---------|
| `inv_webui_01` | `dist/` 不入 Git；`make build` 必先调 `make build-ui`。 | `web/.gitignore` + `Makefile` |
| `inv_webui_02` | npm `dependencies` 仅 `alpinejs` + `marked`。`devDependencies` 仅 `@tailwindcss/vite` + `daisyui` + `vite`。零 CDN 依赖（内网离线可用）。 | `web/package.json` |
| `inv_webui_03` | `FeatureGate.FeatureWebUI=false` 或密封(`SEALED`)态时，API 拒服，UI 出强警告横幅。 | `pkg/substrate/observability/feature_gate.go` |
| `inv_webui_04` | 写操作携带 `X-Session-Token`（`sessionStorage.getItem('polaris_token')`）。 | `web/src/js/sse.js` + middleware |
| `inv_webui_05` | 轮询故障禁止静默（连续 3 次挂出 Warning Banner）。 | `web/src/js/store/statusBar.js` |
| `inv_webui_06` | 日志 SSE 连接不得常驻后台。必须随抽屉关闭而 `disconnect()`。 | `web/src/js/store/logs.js` |
| `inv_webui_07` | Tab 聚合页内每个子 Tab 的数据加载至多触发一次（lazy flag 防重）。 | `web/src/pages/*.html` x-data lazy flags |
| `inv_webui_08` | Chat SSE 状态机仅在 `IDLE → SUBMITTING → THINKING → STREAMING → TOOL_RUNNING → STREAMING → COMPLETE → IDLE` 路径迁移；禁跳态。 | `web/src/js/store/chat.js` 状态转移 |
| `inv_webui_09` | `marked` 渲染必经白名单过滤：清 `script` / `on*` 属性 / `javascript:` URI；`<a>` 强制 `rel="noopener noreferrer"`。零例外。 | `web/src/js/app.js` marked 配置 |
| `inv_webui_10` | `dedupeRunID(sessionID, input)` 在 5s 窗口对相同元组返回同一 runID；防双击/重提交。 | `web/src/js/store/chat.js:dedupeRunID` |
| `inv_webui_11` | Tab 内嵌导航必用 `x-show`（非 `x-if`），保留 DOM 状态防 `setInterval` 在切换时重置。 | `web/src/pages/{monitor,settings,automation}.html` |

---

## 8.6 插件聚合市场（`/plugins` 页）

### DB 表

| 表 | 用途 | 关键字段 |
|----|------|---------|
| `plugin_marketplaces` | 市场源订阅（上游 Registry） | `id, name, type(skill\|mcp), repo_url, trust_tier(3=Official/2=Community), enabled` |
| `catalog_cache` | 市场同步后的目录缓存 | `id, marketplace_id, type(mcp\|skill\|plugin\|app), name, publisher, trust_tier, url, payload(JSON)` |
| `plugins` | 已安装 Plugin 记录（ai-plugin.json 规范） | `id, name, manifest_url, publisher, trust_tier, enabled` |
| `apps` | 已安装 App 记录（独立交互能力） | `id, name, url, publisher, trust_tier, enabled` |
| `skills` (008_skills) | 已安装 Skill / Wasm 记录 | `id, name, wasm_path, trust_tier` |
| `mcp_servers` (018) | 已注册 MCP Server | `id, name, command, args, env, trust_tier` |

### 业务流：安装闭环

`POST /v1/plugins/install` 接收 `{ catalog_id, type }`，后端解析 `catalog_cache` 中对应 payload，写入 `mcp_servers / skills / plugins / apps` 表，并调用 `ToolRegistry.Register()` 热注册（按 `trust_tier` 选择 InProcessSandbox 或 WasmSandbox），返回 `200 { id, status: "installed" }`。卸载走 `DELETE /v1/plugins/{catalogID}`，对应表删除后调用 `ToolRegistry.Unregister()` 热撤销。

### 业务流：市场同步

`POST /v1/plugins/sync` 触发后台异步 goroutine 遍历 `plugin_marketplaces`（`enabled=1`），逐一经 M11 SafeDialer 拉取 `repo_url/index.json`，解析 Manifest 后 UPSERT `catalog_cache`，立即返回 202。客户端轮询 `GET /v1/plugins/catalog` 感知同步完成。

### 安全门控

- `trust_tier=3`（Official）: 直接 WasmSandbox 或 InProcessSandbox，无需额外确认
- `trust_tier=2`（Community）: 安装前显示 Cedar 规则摘要，用户确认后 M11 PolicyGate 签发 Capability Token
- `trust_tier=1`（Unknown）: 默认拒绝，须 Operator 显式 `override_trust_tier`

### 展现层 SSoT 分离 (前端监控页 / Agent 状态)

前端在展示系统可用资源（如 MCP 服务器列表、技能列表）时，必须遵守**配置与状态分离叠加**的渲染契约，特别是针对内嵌在 Plugin 里的组件：
1. **配置名册读库 (Config SSoT)**：列表项的基础信息（名称、Command 等）必须解析底层的 `plugins` / `mcp_servers` 数据表（对于插件内嵌的 MCP，解析 `mcp_policy` 字段获取）。**禁止仅从内存反推配置**。
2. **连接状态读内存 (Status SSoT)**：实时的连接状态（Connected/Disconnected）与运行时的 Error 日志从系统内存的 `runtimeMap` 读取，并附加到配置列表项上进行渲染展示。
3. **消除监控死角**：该设计确保了若某插件由于环境故障（如命令报错、依赖缺失）未能成功注册进内存，前端仍能展示该条记录并高亮其断线与报错状态，避免发生隐身导致的监控盲区。

---

## 8.7 自动化中心（`/automation` 页）

> §跳读: DB 表 / 触发模型 / 执行流 / HITL 审批 / 与聊天的关系 / API / 工作流

### DB 表

| 表 | 用途 | 迁移文件 |
|----|------|---------|
| `automations` | 自动化任务配置（SSoT） | 017 |
| `automation_runs` | 执行历史，每次触发一条记录 | 017 |
| `channels` | Webhook 入站接入（trigger_type=webhook 时关联） | 012 |
| `chat_sessions` | 每次执行生成独立 session（打 automation_id 标签） | 013 |
| `workflows` | 工作流定义（多步骤有序链，含电路断路器） | 029 |
| `workflow_steps` | 步骤有序列表（seq 0-based，可选绑定 automation） | 029 |
| `workflow_runs` | 工作流执行历史（含步骤级产出摘要） | 029 |

**automations 关键字段**：

| 字段 | 说明 |
|------|------|
| `trigger_type` | `cron`=定时 / `webhook`=事件驱动 / `both`=两者 / `manual`=仅手动触发 |
| `cron_schedule` | 标准 5 字段 cron 表达式；trigger_type=webhook 时可空 |
| `channel_id` | 关联 channels.id；webhook 触发时非空 |
| `env_type` | `chat`=纯对话（无目录）/ `local`=读写项目文件 / `worktree`=Git 隔离可生成 PR |
| `projects_json` | JSON 字符串数组，多个项目路径；env_type=chat 时为 `[]` |
| `reasoning_effort` | `low/medium/high/ultra`，自动映射 model_roles（用户不感知模型名） |
| `result_action` | `session`=追加到自动 session / `channel:{id}`=接入推送 / `silent`=静默 |
| `next_run_at` | 预计算下次触发时间，cronTick 按此列索引，避免全表扫描 |
| `last_run_status` | `ok/error/running`，卡片展示上次执行状态 |

**禁止**：`model_id` 不对用户暴露（固定 `auto`），系统根据 `reasoning_effort` 自动映射 model_roles 中的模型。

### 触发模型

| trigger_type | 触发路径 |
|-------------|----------|
| `cron` | `cronTick`（60s 轮询）检查 `next_run_at <= NOW()` → `executeAutomation` |
| `webhook` | `POST /v1/webhooks/{type}/{channelID}` → HMAC-SHA256 验签 → `executeAutomation` |
| `both` | cron 与 webhook 两路均可独立触发，互不干扰 |
| `manual` | `POST /v1/automations/{id}/trigger` → `executeAutomation` |

### 业务流：Cron 后台运行器

`startCronRunner(ctx)` 接收一个可取消 context，由 `Server.Start()` 创建并将取消函数存入 `Server.cronCancel`。`Server.Shutdown()` 首先调用 `cronCancel()` 停止 Cron runner，拒绝新任务触发。

`cronTick()` 每 60s 扫描 `automations` 表中 `next_run_at <= NOW()` 的到期任务，对每条记录启动独立 goroutine 调用 `executeAutomation()`。每次执行写入 `automation_runs` 记录（含 prompt_snapshot）并在结束时更新 `automations.last_run_status` 和 `run_count`。

### 与聊天的关系

- 自动化执行**不注入**用户正在进行的聊天流——避免干扰体验。
- 每次执行产生独立 `chat_sessions` 记录（`source='automation'`，打 `automation_id` 标签），在"会话"页可见（标记🤖）。
- 用户在聊天中说"创建一个每天整理 PR 的自动化" → M4 Agent 识别意图 → 引导跳转自动化页面或调用内置工具直接创建（M4 职责，不在本模块）。
- 用户在自动化任务的执行历史中可点击"查看会话记录"跳转对应 session，继续与 Agent 对话。

### 业务流：HITL 审批

Agent 执行触发危险操作（`write_network` / Privileged / 超预算）时，M11 Cedar-Gate 拦截并将 `tasks.status` 置为 `Suspended`，通过 SSE 推送 `approval_pending` 事件使前端 approvals badge +1。用户进入自动化页"待办审批" Tab，调用 `GET /v1/approvals/pending` 查看意图与风险后，提交 `POST /v1/approvals/{id}/resolve { decision, note? }` 完成审批，M8 Blackboard 相应将 `TaskEntry.status` 更新为 `Running` 或 `Cancelled`。

### 外部触发器（Webhook）

`POST /v1/webhooks/{channelType}/{channelID}` → M13 ChannelManager → 查 `automations WHERE channel_id=channelID AND enabled=1` → HMAC-SHA256 验签（密钥存 CredentialVault，验签失败 401）→ `executeAutomation(..., "webhook")`。

> **已修复（webhook body 大小限制）**：原实现未限制请求体大小，攻击者可发送超大 body 耗尽内存。当前已改为 `MaxBytesReader` 限制 4MB，超限返回 413。

> **已修复（健康端点豁免收窄）**：`/healthz`、`/readyz` 此前豁免范围过宽。当前已收窄为严格路径匹配，仅 `GET /healthz` 和 `GET /readyz` 豁免认证，其余路径不受影响。

### API 汇总

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/v1/automations` | 列出全部自动化任务 |
| POST | `/v1/automations` | 创建（`trigger_type / cron_schedule / env_type / projects_json / reasoning_effort / result_action`） |
| PUT | `/v1/automations/{id}` | 更新（patch 语义） |
| DELETE | `/v1/automations/{id}` | 删除（同时删除 `automation_runs`） |
| GET | `/v1/automations/{id}/runs` | 执行历史（`limit` 默认 20） |
| POST | `/v1/automations/{id}/trigger` | 手动立即触发 |

### 工作流（Workflows）

工作流是 automations 的**有序链式扩展**：将多个 Agent 任务串联为顺序流水线，前一步产出自动注入为下一步上下文。

**核心设计**：
- 触发：`cron`（由 `cronTick` 同批次调用 `cronTickWorkflows`）/ `manual`
- 数据交接：上一步 reply 文本截断至 2000 字符后注入下一步 prompt 前缀（`[前一步骤输出]...`）
- 每步产生独立 chat_session，执行历史写入 `workflow_runs.step_outputs`（JSON 数组）
- 电路断路器复用 `automations` 同策略（连续 5 次 error → circuit_open=1）
- 步骤可选绑定已有 `automation_id`，或使用内联 prompt（两者取其一）

**触发流**：`cron` 由 `cronTickWorkflows`（60s）驱动，`manual` 由 `POST /v1/workflows/{id}/trigger` 触发，两者均调用 `executeWorkflow` 顺序执行各步骤。

**API**：

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/v1/workflows` | 列出全部工作流 |
| POST | `/v1/workflows` | 创建（含 `steps` 数组） |
| GET | `/v1/workflows/{id}` | 获取详情（含步骤列表） |
| PUT | `/v1/workflows/{id}` | 更新（`steps` 非 nil 时全量替换） |
| DELETE | `/v1/workflows/{id}` | 删除（级联删除 steps + runs） |
| GET | `/v1/workflows/{id}/runs` | 执行历史（含步骤级输出摘要，limit 30） |
| POST | `/v1/workflows/{id}/trigger` | 手动立即触发 |

---

## 8.8 设置：权限模式与电脑操控（`/settings` → `电脑操控` Tab）

### DB 表

`preferences` 表（`key TEXT PK, value TEXT`）存储所有偏好，computer use 相关键：

| key | value 示例 | 说明 |
|-----|-----------|------|
| `computer_use.permission_mode` | `"safe"\|"workspace"\|"god"` | 全局权限模式 |
| `computer_use.perception_mode` | `"auto"\|"local_omniparser"\|"cloud_vlm"` | 视觉感知引擎策略（允许高性能机器强制走云端大模型，如 DeepSeek V4 多模态） |
| `computer_use.allowed_applications` | `["chrome","vscode","terminal"]` | 受控应用白名单（JSON 数组） |
| `computer_use.chrome_cdp_enabled` | `"true"` | 是否启用 Chrome CDP 深度交互 |

### 权限模式语义

| 模式 | Cedar 能力 | 对应 M7 SandboxTier |
|------|-----------|-------------------|
| `safe` | ReadOnly + 沙箱执行 | InProcess / Wasm(L2) |
| `workspace` | ReadOnly + WriteLocal（白名单目录） | Wasm(L2) |
| `god` | 全能力（含 WriteNetwork / Privileged / Computer Use） | Container(L3) + Accessibility API |

### 业务流：模式切换与赋权

`PUT /v1/preferences/computer_use.permission_mode` 接收 `{ value: "safe" | "workspace" | "god" }`，后端 UPSERT `preferences` 表并热更新 `Agent.Config`，由 M11 PolicyGate 为当前 Session 重新签发对应权限级别的 Capability Token，返回 200 `{ key, value }`。

### 业务流：模式与应用偏好配置

`PUT /v1/preferences/computer_use.allowed_applications` 将白名单 JSON 数组持久化到 `preferences` 表，仅供前端展示用。当前 `interceptComputerUse()`（`pkg/cognition/kernel/agent_execute.go`）实际读取 `computer_use_mode` 决定是否触发 HITL，`allowed_applications` 为 UI 存储键，未来可扩展为应用级细粒度控制（详见 M07 §7.1）。

> **注**：`interceptComputerUse()`（`pkg/cognition/kernel/agent_execute.go`）实际读取 `computer_use_mode`（`auto_review`/`default`）决定是否触发 HITL，详见 M07 §7.1。`allowed_applications` 持久化于 `preferences` 表供前端展示，未来可扩展为应用级细粒度控制。

### Chrome CDP 深度交互

当 `computer_use.chrome_cdp_enabled=true` 且 `chrome` 在 allowed_applications 中：
- Agent 工具调用 `browser_use` / `computer_use` → `interceptComputerUse()` 放行
- 通过 MCP puppeteer 或 Chrome DevTools Protocol 执行 `click(x,y)` / `type(text)` / `read_dom()`
- 所有 DOM 读取内容进入 `TaintLevel=High`（外部 Web 内容，inv_M7_02）

---

## 8.9 前端组件规范（官方推荐写法）

> 约束：所有 UI 组件**优先采用 DaisyUI / Tailwind / Alpine.js 官方文档推荐的原生结构**；禁止自造等效轮子、禁止在 `style.css` 中重复实现已有组件语义。以下为已落地的规范对照。

| 场景 | 官方组件/写法 | 禁止写法 |
|------|-------------|---------|
| **弹窗 (Modal)** | `<dialog class="modal">` + `<div class="modal-box">`，关闭用 `<form method="dialog">` 或 `modal-backdrop` 点击事件 | 自定义 `position:fixed` 遮罩层 |
| **标签页 (Tabs)** | `<div role="tablist" class="tabs tabs-boxed">` + `<a role="tab" class="tab">` 原生结构，激活态仅加 `tab-active`，不额外叠加自定义背景色 | `tab-active bg-base-200 shadow-sm` 等手工覆盖导致圆角失效 |
| **抽屉 (Drawer)** | DaisyUI `drawer` 布局：`<div class="drawer">` → `<input type="checkbox" class="drawer-toggle">` → `<div class="drawer-content">` → `<div class="drawer-side">` → `<label class="drawer-overlay">` | CSS `transform: translateX` 手工侧滑 |
| **聊天气泡** | `<div class="chat chat-start/chat-end">` + `<div class="chat-bubble">` | 自定义 flexbox 对齐 + 自定义气泡背景 |
| **Flexbox 页面滚动** | 所有 `overflow-y-auto` 滚动容器必须同时加 `min-h-0`，否则 Flex 子元素撑破父容器导致原生惯性滚动失效 | 仅设 `overflow-y-auto` 而不加 `min-h-0` |
| **表单字段** | `<label class="form-control">` + `<div class="label"><span class="label-text">` + `<input class="input input-bordered">` | 自定义 `.form-group / .form-label / .form-input` class |
| **Badge / Tag** | `<div class="badge badge-{variant} badge-{size}">` | 手工 `border-radius + padding + color` |
| **空状态 (Empty State)** | `<div class="flex flex-col items-center justify-center">` + SVG icon + DaisyUI 排版 class | 自定义空态组件 |
| **多语言文本** | 所有用户可见字符串通过 `x-text="$store.i18n.t('key')"` 或 `:placeholder="$store.i18n.t('key')"` 绑定，中英文 key 分别在 `i18n.js` 的 `zh` / `en` 字典注册 | 硬编码中文字符串直接写入 HTML 模板 |
| **Emoji + 翻译文本拼接** | Emoji 在 HTML 模板中拼接：`x-text="'📦 ' + $store.i18n.t('key')"`；i18n value 只含纯文字，不含 emoji | i18n value 中混入 emoji 导致模板再次拼接后重复出现 |
| **全局 CSS** | `web/src/css/style.css` 仅含：Tailwind v4 初始化、DaisyUI 主题声明、少量设计 Token（`@theme`）、全局 CSS 变量覆盖（`--radius-box`），不写任何组件样式 | 在 `style.css` 中重新实现 DaisyUI 已有组件 |

**补充约束**（`inv_webui_12` ~ `inv_webui_14`）：

| ID | 约束 |
|----|------|
| `inv_webui_12` | 任何新 Tab 标签页容器必须用 `tabs tabs-boxed`，激活项仅加 `tab-active`，不额外叠加自定义背景色或阴影，保证 DaisyUI 主题下圆角曲率正确渲染。 |
| `inv_webui_13` | Flex 布局下的滚动容器（含各子页面顶层 `<div>`）必须同时携带 `overflow-y-auto min-h-0`，缺一不可。 |
| `inv_webui_14` | 所有新增用户可见字符串必须同时在 `zh` 和 `en` 字典注册对应 key，不允许仅注册其中一个语言版本后上线。 |
