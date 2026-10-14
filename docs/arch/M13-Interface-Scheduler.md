
# 模块 13: Interface & Scheduler

> 对外: CLI + HTTP/SSE + MCP + Web UI + 桌面外壳; 对内: 任务队列 + 定时任务 + HITL
> Go; [HE-Rule-1]; [Tier-0-Limit]; [Phase0-Bootstrapping]
<!-- §跳读: 0-bis:7 职责 / 0-ter:22 不变量速查 / 1:36 对外接口 / 2:361 对内调度 / 3:450 MCP / 6:458 (SOFT)降级 / 6-bis:471 已知Bug修复记录 / 7:483 跨模块契约 / 8:500 Web UI 规约 / 8.6:614 834 插件聚合市场DB+流 / 8.7:645 870 自动化中心DB+流+工作流 / 8.8:750 987 电脑操控权限+Preferences / 8.9:767 1027 前端组件规范 -->
## 0-bis. 职责边界

| M13 **是** | M13 **不是** |
|-----------|-------------|
| 对外接口层（CLI + HTTP REST + SSE + Web UI + 桌面外壳） | 业务逻辑执行（各模块负责） |
| 对内调度（TaskQueue + Cron + ResourceGovernor） | 任务分解与编排（M8） |
| HITL 审批网关（HITLGateway + Notifier） | 审批策略制定（M11 [ESCALATE]） |
| TrafficSplitter 流量分发执行（percent 控制，设计；见 §2.5） | 流量切换决策（M9 ProgressiveRollout） |
| ResourceGovernor 准入控制（三级降级联合执行） | 内存压力检测（M3 OSMemoryGuard） |
| Sealed/Unsealed 服务器状态管理 | KillSwitch 阶段触发（M11） |
| 对外 API 认证（Session Token + API Key） | 凭证存储（M11 CredentialVault） |
| EgressGateway Provider 域名白名单预检 | 网络连接安全（委托 M11 SafeDialer.DialContext） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M13_01 | 默认绑定 127.0.0.1——远程绑定须显式 + TLS + capability + audit | M13 §1.2.1 AuthMiddleware |
| inv_M13_02 | 单机优先——不引入 Kafka/Redis/RabbitMQ/K8s | 架构硬约束审计 |
| inv_M13_03 | Worker pool 严格隔离——intent_handler/eval/ingest/background/cron 独立 pool | M13 §2.0 ResourceGovernor |
| inv_M13_04 | HITL 审批超时触发 timeout policy——kill_pause（默认）/ auto_deny / auto_approve | M13 §2.4 ApprovalRequest |
| inv_M13_05 | ResourceGovernor 与 M3 OSMemoryGuard 共享三级降级阈值——任一触发即执行 | M13 §2.0 三级降级 |
| inv_M13_06 | 所有出站请求经 EgressGateway → M11 SafeDialer.DialContext 完整五阶段 SSRF 防护 | M13 §1.2.2 EgressGateway ✅ 已实现 |
| inv_M13_07 | 电脑操控 checkpoint（CheckpointDeviceControlReview）超时兜底与"设置→设备操控"权限模式联动：仅 full_access 兜底为 auto_approve；TaintLevel>=Medium 硬地板不受权限模式影响 | M13 §2.4 权限模式联动 |

---

## 1. 对外接口

### 1.1 CLI

CLI 是**薄客户端**（ADR-0096 决策一）：全部命令经 HTTP/SSE 调用守护进程，不直连数据库与内核；`internal/cli` 已按 ADR-0096 决策六删除，实现全在 `cmd/polaris/`（分发见 `cmd/polaris/main.go`）。

- 服务发现与凭证注入 `cmd/polaris/cli_endpoint.go`：`POLARIS_SERVER_URL` → `run/polaris.port` + `run/polaris.token` → 兜底 `localhost:28888`；端口不写死，守护进程可配 `port = 0` 由内核分配。
- 子命令：`serve` / `service install|uninstall|status` / `init|setup` / `chat` / `project` / `status` / `usage` / `config get|set|budget` / `export` / `import` / `migrate openclaw` / `memory process-staging` / `vault` / `peer` / `skill` / `eval` / `csv-fanout` / `allowlist` / `unseal` / `seal-status` / `release-key` / `version`。
- REPL（`cmd/polaris/cli.go`）：逐行读 stdin，`/` 前缀 → 内置命令（`/new` `/sessions` `/clear` `/help` `/quit`），否则 → `POST /v1/agent/stream`，SSE token → stdout。
- `polaris service` 把守护进程注册给系统服务管理器（launchd LaunchAgent / systemd --user / Windows 计划任务），一律用户级——数据目录在用户 home 下，以 root 跑会把文件属主弄成 root。
- 数据导出/迁移：`polaris export [outfile.jsonl]` 流式导出 `chat_sessions` / `chat_messages` / `kv_store`(config 前缀)；`polaris import <infile.jsonl>` 幂等 upsert。
- **外部平台迁移** `polaris migrate openclaw`（`cmd/polaris/migrate_openclaw.go`，从 ~/.openclaw 导入）：配置与密钥 → `configs/defaults.toml`；SOUL.md/AGENTS.md → 系统 prompt + M5 记忆层；记忆 SQLite → EventLog（`--with-memory`）；`--stage`（默认）写隔离命名空间 `salience=0.3`，`--smart` 启用 LLM 预压缩；迁移后 `polaris memory process-staging` 触发去重→Salience 重算→提升主线；技能 SKILL.md 仅拷贝源码并标注"需人工编译为 Wasm"。
- 月度成本报告：cron `0 0 1 * *` → `monthly_cost_report.md`（by_provider / by_task_type / by_session / by_call_type 四维度），实现 `internal/automation/cost_report.go`：聚合上月 `llm.call.recorded` 事件的 input/output tokens × 内置 provider 成本系数；DB 不可用降级为空报告。`polaris config budget set <amount>` 写 `kv_store` 键 `config:budget:monthly_usd`。

### 1.1.1 桌面外壳

`desktop/`（Tauri v2，ADR-0096）与 CLI 同为薄客户端：窗口直接加载 `http://127.0.0.1:<port>`，跑 `web/` 那套 UI；外壳不打包前端资源、不承载业务 IPC。

- 启动判定（`desktop/src-tauri/src/startup.rs`）：找 polaris 二进制 → `polaris service status --json` 取运行时状态 → 未运行则宿主模式拉起 + 指数退避轮询；已运行则**两段式探测**（`/healthz` 判存活、带令牌端点判凭证）后附着。路径解析问 Go 二进制，SSoT 仍是 `config.DataLayout`。
- 外壳内只有两张纯静态页（启动页、故障页）；故障页覆盖「核心未安装 / 启动超时 / 凭证不匹配 / 运行时状态异常」四态，不调用 `/v1`。生命周期见 ADR-0096 决策七；四场景由 `make desktop-smoke` 实跑校验。

### 1.2 HTTP REST API

路由事实源：`internal/gateway/server/server_routes.go`，权威全表见 §1.2.9（生成）。下表按业务域给语义分组与跨小节引用。

| 业务域 | 端点（前缀） | 说明 |
|---|---|---|
| 系统 | `GET /healthz` `/readyz` `/metrics` `/v1/status` `/v1/doctor` `/v1/config/server` `/v1/system/{version,capabilities,update}` | 健康/就绪/指标/状态（Agent 状态 + Token 统计 + 内存）/诊断；`/_admin/{kill,unseal}` 为 KillSwitch 管理面 |
| Agent 对话 | `POST /v1/agent/query`（同步）`/v1/agent/stream`（SSE）`/v1/agent/codeact`；`GET /v1/agent/tasks/{taskID}`；`POST /v1/agent/{taskID}/interrupt`（§1.2.5）；`GET /v1/logs/stream`（日志 SSE） | |
| 会话/项目 | `/v1/sessions`（列表/详情/删除）`GET /v1/sessions/{id}/context`（§1.2.6）`POST …/recap` `PUT …/project`；`/v1/projects`（CRUD，ADR-0097）；`GET /v1/search` `/v1/insights` | |
| 用量/预算 | `GET /v1/usage`（`?since=24h&until=&group_by=purpose\|model\|provider\|day`；请求数/输入·缓存命中·输出·推理 token/命中率/估算费用；ADR-0105 决策八，CLI `polaris usage`）`/v1/config/budget`（GET/PUT） | |
| Provider/模型 | `/v1/providers`（CRUD + `/from-catalog` + `/{id}/test`）`/v1/providers/{id}/models`（CRUD + deprecate/upgrade）`/v1/catalog/providers`；`/v1/config` `/v1/config/model-roles`（GET/PUT） | |
| 工具/技能 | `GET /v1/tools`、`POST /v1/tools/{name}/execute`；`/v1/skills`（list/install/create/commands/injections）；`/v1/hooks`（含 `hooks/trust`） | |
| MCP/插件/市场 | `/v1/mcp-servers`（CRUD + test + network-access + oauth*）`/v1/mcp/create` `/v1/mcp-apps/*` `/v1/elicitations`；`/v1/plugins/*`（catalog/sync/install/create/marketplaces/channels/prune，`/{id}` 的 config/apps/dependencies/upgrade/`mcp/{serverName}/toggle`）；`/oauth/mcp/callback` `/oauth/client-metadata.json` | 扩展体系见 ADR-0103 与 M13-bis；DB 与流程见 §8.6 |
| 接入（Channel） | `/v1/channels`（CRUD）；`/v1/webhooks/{channelType}/{channelID}`（入站） | §8.7 外部触发器 |
| 自动化/工作流 | `/v1/automations`（CRUD + `/runs` + `/trigger`）`/v1/automation-templates` `/v1/workflows`（CRUD + `/runs` + `/trigger` + `runs/{id}/resume` + `runs/{id}/data/{step_key}`） | §8.7 |
| 编排管理 | `POST /v1/admin/tasks/{pattern-mapreduce,pattern-parallel,pattern-sequential,pattern-swarm,pattern-dag,pipeline,csv-fanout}`；`GET /v1/agents` | |
| HITL | `GET /v1/approvals/pending`；`POST /v1/approvals/{id}/resolve`（approve/deny） | §2.4 |
| 多节点 | `/v1/mesh/{self,peers,invites,tokens,settings}`（含 `PUT /v1/mesh/settings`，设置页「多节点」Tab）；`GET /.well-known/agent-card.json`；`/v1/a2a/tasks`；对外 A2A 1.0 门面 `/a2a/v1` | ADR-0111；实现 `internal/gateway/mesh/` 与 `internal/gateway/server/meshapi/` |
| 偏好/电脑操控 | `GET /v1/preferences`；`PUT /v1/preferences/{key}`（含 `computer_use_mode`、`permission_mode`） | §8.8 |
| 语音 | `POST /v1/audio/{speech,transcriptions,stt/install,tts/install}`；`GET /v1/audio/status` | §8.4，ADR-0110 |
| 评测/嵌入 | `POST /v1/eval/{run,benchmark}`；`/v1/eval/meta-audit`；`POST /v1/embedding/rebench` | M12，ADR-0109 |
| 导出/导入 | `GET /v1/export/{trajectories,backup}`；`POST /v1/import/backup`（幂等 upsert）；`POST /v1/workspace/upload`；`POST /v1/vault/rotate-master-key` | |
| OpenAI 兼容 | `POST /v1/chat/completions` | 第三方客户端接入 |

**SSE 请求**（`POST /v1/agent/stream`）：query(string,req) / session_id / model / temperature(float32) / max_tokens(int)，后四项 opt。
**SSE 事件**（text/event-stream，`data: <JSON>\n\n`）：`token` | `tool_call` | `tool_result` | `thinking` | `error` | `complete` | `context_warning` | `status/compacting` | `status/compacted`。
- `context_warning`：上下文使用率 ≥ warnPct 触发；payload `{usage_pct, thrashing}`（thrashing=true → 前端红色 Banner 并隐藏"立即压缩"，见 §1.2.6）。
- `status/compacting` / `status/compacted`：压缩进行中（蓝色 Indicator）/ 完成（末尾消息标分隔线）。

**SessionOrchestrator 领域层**（ADR-0066 决策五，原 ADR-0085）：`POST /v1/agent/stream` 与 `AgentPool.AcquireHeadless`（Cron/Workflow/Webhook）两条会话轮次入口共享 `internal/gateway/session.Orchestrator.RunTurn`，会话确保/历史加载/Hook 分发/斜线命令路由/压缩决策/`SystemPromptGuard` 装配/消息持久化统一收敛于此。HTTP 层只做请求解码/SSE 帧转译/错误码映射（`chat/sse_sink.go` 实现 `session.Sink`）。`internal/gateway/session/` 零 `net/http` 依赖（`Test_inv_M13_SessionPkgNoHTTP` 门控）；驱动 FSM 轮次的 `SendIntent(types.TriggerIntentReceived)` 调用点收敛至三处固定位置（`Test_inv_M13_SingleTurnEntry` 门控）。

#### 1.2.1 认证

**AuthMiddleware**:
- X-Session-Token → 匹配本地 Bearer Token（`~/.polaris/.session_token` 0600）→ 放行；loopback 不免密 → 401
- X-API-Key → `[CredentialVault] KeychainProvider.Verify SHA-256` 常量时间比较 → 失败 401
- **公网**: JWT Ed25519 + TLS 1.3；3 次失败 → IP 冷却 5min
- 对端节点令牌与 admin 密钥分离（ADR-0111 决策三）；`/healthz`、`/readyz` 仅严格路径匹配 `GET` 豁免认证。

#### 1.2.2 Egress Gateway

HTTP 层出站适配器（`internal/gateway/egress/`，`EgressGateway` 实现 `http.RoundTripper`，白名单原子更新，注入点 `cmd/polaris/boot_substrate.go`），委托 M11 SafeDialer（M11 §6）执行完整 SSRF 防护（DNS 解析、CIDR 校验、TOCTOU 消除、IP 锁定）。本层仅做 Provider 域名白名单预检（api.deepseek.com, api.anthropic.com, api.openai.com, api.github.com），不在白名单提前拒绝以省 DNS 开销。**默认白名单不含 `localhost`/`127.0.0.1`**（ADR-0066 决策三，纵深防御）；本地调试经 `/config network allow example.com:443` 动态追加，仍需过 SafeDialer；用户自托管的可观测导出端点（LangSmith/Braintrust/Phoenix，M03 §9.1）同样走此白名单，默认不放行。Downloader 的公网出站豁免见 ADR-0066 决策六。

**方法级能力防护**：出站请求还经 `internal/security/network/safe_dialer_capability.go` 的 `WrapCapability(inner, cap)` 封装，按 `CapabilityType`（如 `CapNetworkRead`）在 RoundTripper 层终检，拦截非 GET/HEAD/OPTIONS 方法；已接入 `web_fetch` / `web_search` 出站路径。

#### 1.2.3 Sealed 状态

`/v1/status` 的 `sealed` 字段由 `observability.GlobalKillswitchStage` 原子量驱动（`>= KillFullStop(3)` 为 `true`），KillSwitch 状态变迁经 `StateChangeCallback` 同步写入。KillFullStop 触发后写 `dataDir/.fullstop`，重启时 `main.go` 检测到即拒绝启动（M11 §4.2）。

#### 1.2.4 优雅关闭

`Server.Shutdown(ctx)` 顺序：
1. `cronCancel()` — 停止 Cron runner（拒绝新触发，运行中的 automation goroutine 持自身超时 ctx 自然结束）
2. `channelMgr.StopAll()` — 停止所有聊天平台 poller
3. `srv.Shutdown(ctx)` — 排空进行中 HTTP 请求，超时 30s

`main.go` 在 Shutdown 后等待 DatabaseWriter flush 残余批次（dbWriterDone channel）。

#### 1.2.5 `[UserInterrupt]` 端点（inv_global_08，<200ms）

`POST /v1/agent/{taskID}/interrupt`，Body：`{ action: "resume" | "redirect" | "abort", redirect?: string, reason?: string }`。

**协议**：Auth(X-Session-Token) 验证后，经 MutationBus 写 pending 标记并向 EventLog 推 `agent_interrupt_requested`，触发 M4.ContextCancel() 取消所有子 goroutine，立即返回 202（不等 S_INTERRUPT 确认）。跨 session 中断需 `interrupt_remote` Capability。
**SLO**：端点接收 → ContextCancel 完成 <200ms（M3 `polaris_user_interrupt_latency_ms` Histogram）。

**action 语义**:
- `resume`: `reason` 可附日志说明，Agent 恢复原状态继续
- `redirect`: `redirect` 字段注入新目标（ZoneImmutable，TaintUserReviewed），跳转 S_PLAN 重新规划，不消耗 ReplanCount
- `abort`: 进入 S_FAILED + Saga 逆序补偿 + workspace GC

**约束**:
- 与 [KillSwitch] 同优先级但作用域为单 task；KillSwitch FULLSTOP 覆盖所有 task
- 同 task 30s 内重复中断 → HTTP 429（防抖）
- task 不存在 / 已 S_COMPLETE/S_FAILED → HTTP 404

#### 1.2.6 上下文压缩机制（Compressor）

`internal/gateway/server/chat/compression_service.go`（`CompressionService`），对齐 Claude Code 自动压缩。阈值为百分比而非绝对 token 数，TOML `configs/defaults.toml [compressor]`：

| 参数 | 默认值 | 含义 |
|------|--------|------|
| `context_window` | 32768 | 上下文窗口（token） |
| `auto_compact_pct` | 95% | 自动压缩阈值（`contextWindow × pct`） |
| `warn_pct` | 80% | 警告 Banner 阈值 |
| `max_thrash_count` | 3 | 连续 thrashing 上限 |

- **Thrashing 防抖**：压缩后仍超阈值则累计 `thrashedCount`；连续 ≥ `max_thrash_count` 次停止自动压缩，SSE 推红色 `context_warning{thrashing:true}` 等用户手动干预（`ForceCompact()` 重置计数）。
- **调用链**：`sse.go` 每轮推理前 `NeedsCompact()` → thrashing 状态跳过 → 否则 `Compact()`（LLM 摘要），前后各推一次 `status/compacting` / `status/compacted`。
- `GET /v1/sessions/{id}/context` 返回 `ContextStats`（token 数/阈值/使用率/上次压缩时间/thrashing），供前端 `/context` 命令展示。

#### 1.2.7 消息预处理：引用展开（ContextRefExpander）

`internal/gateway/authcontext/contextref.go`：SSE 管道最早阶段（请求基础校验后、`SlashCommandRouter.Dispatch` 前）展开用户输入中的 `@file`/`@url`/`git:` 引用，结果写回 `req.Input` 再进入斜线命令、Compressor。单条展开失败记入 `report.Skipped` 并 `slog.Warn`，不阻断；`ContextRefExpander` 为 nil（未注入 HTTPClient）时整段跳过。

#### 1.2.8 斜线命令系统（SlashCommandRouter）

`internal/gateway/server/chat/slash_commands.go`：在 SSE 层拦截输入（引用展开之后、provider 选取后、LLM 推理前），不消耗 TokenBudget，不进入 Agent FSM。

| 命令 | 行为 |
|------|------|
| `/context` | 读 `ContextStats` → SSE 输出 token 使用率表 |
| `/compact` | `ForceCompact()`（跳过阈值检查 + 重置 thrashing 计数） |
| `/clear` | 物理删除 DB 非 system 消息 + 前端 `clearView()` 双清 |
| `/help` | 输出所有注册命令 |

另有 `/steer`（`slash_command_steer.go`）与技能命令（`/插件:技能 参数`，`slash_skills.go`，见 M13-bis §3.1）。`Dispatch()` 返回 `CommandResult{Handled, Response, UpdatedHistory, RewrittenInput}`；Handled=true 调用方短路并直接发 `complete`，命令回复作为助手消息持久化。

#### 1.2.9 路由完整清单（生成，交叉核对用）

上方 §1.2 分组表手写，携带语义分组与跨小节引用；下表由 `tools/docs_gen.go` 从 `server_routes.go` 机械提取，是路由漂移的权威核对基准（`make docs-gen-check` 门控）。两者不一致时，判断"代码里有什么"以下表为准，判断"端点属于哪个业务域"以分组表为准。

<!-- BEGIN GENERATED: m13-route-inventory · 源: internal/gateway/server/server_routes.go · 勿手改，改源后跑 make docs-gen -->
| Method | Path | Handler |
|---|---|---|
| GET | `/.well-known/agent-card.json` | `serveMesh` |
| POST | `/_admin/kill` | `sysadminHandler.HandleKill` |
| POST | `/_admin/unseal` | `sysadminHandler.HandleUnseal` |
| GET | `/healthz` | `handleHealthz` |
| GET | `/metrics` | `metrics.MetricsHandler(s.tbr)` |
| GET | `/oauth/client-metadata.json` | `HandleMCPOAuthClientMetadata` |
| GET | `/oauth/mcp/callback` | `HandleMCPOAuthCallback` |
| GET | `/readyz` | `handleReadyz` |
| POST | `/v1/a2a/tasks` | `a2a.TaskSubmitHandler(s.blackboard)` |
| POST | `/v1/admin/tasks/csv-fanout` | `sysadminHandler.HandleCSVFanout` |
| POST | `/v1/admin/tasks/pattern-dag` | `sysadminHandler.HandlePatternDAGRun` |
| POST | `/v1/admin/tasks/pattern-mapreduce` | `sysadminHandler.HandleMapReduceRun` |
| POST | `/v1/admin/tasks/pattern-parallel` | `sysadminHandler.HandleParallelRun` |
| POST | `/v1/admin/tasks/pattern-sequential` | `sysadminHandler.HandleSequentialRun` |
| POST | `/v1/admin/tasks/pattern-swarm` | `sysadminHandler.HandleSwarmRun` |
| POST | `/v1/admin/tasks/pipeline` | `sysadminHandler.HandlePipelineRun` |
| POST | `/v1/agent/codeact` | `handleCodeAct` |
| GET | `/v1/agent/mmd-canvas` | `sysadminHandler.HandleGetMMDCanvas` |
| POST | `/v1/agent/query` | `handleAgentQuery` |
| POST | `/v1/agent/stream` | `chatHandler.HandleAgentStream` |
| GET | `/v1/agent/tasks/{taskID}` | `handleGetAgentTask` |
| POST | `/v1/agent/{taskID}/interrupt` | `handleAgentInterrupt` |
| GET | `/v1/agents` | `pluginHandler.HandleListAgents` |
| POST | `/v1/approvals/` | `handleResolveApproval` |
| GET | `/v1/approvals/pending` | `handleGetPendingApprovals` |
| POST | `/v1/audio/speech` | `chatHandler.AudioService.HandleAudioSpeech` |
| GET | `/v1/audio/status` | `handleGetAudioStatus` |
| POST | `/v1/audio/stt/install` | `chatHandler.AudioService.HandleAudioInstall("stt")` |
| POST | `/v1/audio/transcriptions` | `chatHandler.AudioService.HandleAudioTranscriptions` |
| POST | `/v1/audio/tts/install` | `chatHandler.AudioService.HandleAudioInstall("tts")` |
| GET | `/v1/automation-templates` | `sysadminHandler.Cron.HandleListAutomationTemplates` |
| GET | `/v1/automations` | `sysadminHandler.Cron.HandleListAutomations` |
| POST | `/v1/automations` | `sysadminHandler.Cron.HandleCreateAutomation` |
| DELETE | `/v1/automations/{id}` | `sysadminHandler.Cron.HandleDeleteAutomation` |
| PUT | `/v1/automations/{id}` | `sysadminHandler.Cron.HandleUpdateAutomation` |
| GET | `/v1/automations/{id}/runs` | `sysadminHandler.Cron.HandleListAutomationRuns` |
| POST | `/v1/automations/{id}/trigger` | `sysadminHandler.Cron.HandleTriggerAutomation` |
| GET | `/v1/catalog/providers` | `providerHandler.HandleListCatalogProviders` |
| GET | `/v1/channels` | `sysadminHandler.Channels.HandleListChannels` |
| POST | `/v1/channels` | `sysadminHandler.Channels.HandleCreateChannel` |
| DELETE | `/v1/channels/{channelID}` | `sysadminHandler.Channels.HandleDeleteChannel` |
| PUT | `/v1/channels/{channelID}` | `sysadminHandler.Channels.HandleUpdateChannel` |
| POST | `/v1/chat/completions` | `sysadminHandler.HandleOpenAIChat` |
| GET | `/v1/config` | `handleGetConfig` |
| GET | `/v1/config/budget` | `sysadminHandler.HandleGetBudget` |
| PUT | `/v1/config/budget` | `sysadminHandler.HandleSetBudget` |
| GET | `/v1/config/model-roles` | `providerHandler.HandleGetModelRoles` |
| PUT | `/v1/config/model-roles` | `providerHandler.HandleSetModelRoles` |
| GET | `/v1/config/server` | `handleGetConfig` |
| GET | `/v1/doctor` | `sysadminHandler.HandleDoctor` |
| GET | `/v1/elicitations` | `handleGetElicitations` |
| POST | `/v1/elicitations/{id}` | `handleRespondElicitation` |
| POST | `/v1/embedding/rebench` | `handleEmbeddingRebench` |
| POST | `/v1/eval/benchmark` | `sysadminHandler.Eval.HandleBenchmark` |
| GET | `/v1/eval/meta-audit` | `sysadminHandler.Eval.HandleGetMetaAuditStatus` |
| POST | `/v1/eval/meta-audit` | `sysadminHandler.Eval.HandleRunMetaAudit` |
| POST | `/v1/eval/meta-holdout/cases` | `sysadminHandler.Eval.HandleAddMetaHoldoutCase` |
| POST | `/v1/eval/run` | `handleEvalRun` |
| GET | `/v1/export/backup` | `sysadminHandler.HandleExportBackup` |
| GET | `/v1/export/trajectories` | `sysadminHandler.HandleExportTrajectories` |
| GET | `/v1/hooks` | `pluginHandler.HandleListHooks` |
| DELETE | `/v1/hooks/trust` | `pluginHandler.HandleRevokeHookTrust` |
| POST | `/v1/hooks/trust` | `pluginHandler.HandleTrustHook` |
| POST | `/v1/import/backup` | `sysadminHandler.HandleImportBackup` |
| GET | `/v1/insights` | `sysadminHandler.HandleInsights` |
| GET | `/v1/logs/stream` | `handleLogStream` |
| GET | `/v1/mcp-apps/config` | `handleGetMCPAppsConfig` |
| GET | `/v1/mcp-apps/resource` | `handleGetMCPAppsResource` |
| PUT | `/v1/mcp-apps/views/{viewID}/model-context` | `handleMCPAppsViewModelContext` |
| POST | `/v1/mcp-apps/views/{viewID}/rpc` | `handleMCPAppsViewRPC` |
| PUT | `/v1/mcp-apps/views/{viewID}/state` | `handleMCPAppsViewState` |
| GET | `/v1/mcp-servers` | `sysadminHandler.MCP.HandleListMCPServers` |
| POST | `/v1/mcp-servers` | `sysadminHandler.MCP.HandleCreateMCPServer` |
| DELETE | `/v1/mcp-servers/{serverID}` | `sysadminHandler.MCP.HandleDeleteMCPServer` |
| PUT | `/v1/mcp-servers/{serverID}` | `sysadminHandler.MCP.HandleUpdateMCPServer` |
| PUT | `/v1/mcp-servers/{serverID}/network-access` | `sysadminHandler.MCP.HandleMCPNetworkApproval` |
| PUT | `/v1/mcp-servers/{serverID}/oauth` | `sysadminHandler.MCP.HandlePutMCPServerOAuth` |
| POST | `/v1/mcp-servers/{serverID}/oauth/authorize` | `sysadminHandler.MCP.HandleAuthorizeMCPServer` |
| DELETE | `/v1/mcp-servers/{serverID}/oauth/token` | `sysadminHandler.MCP.HandleDeleteMCPServerOAuthToken` |
| POST | `/v1/mcp-servers/{serverID}/test` | `sysadminHandler.MCP.HandleTestMCPServer` |
| POST | `/v1/mcp/create` | `pluginHandler.HandleCreateMCP` |
| POST | `/v1/mesh/invites` | `handleMeshCreateInvite` |
| GET | `/v1/mesh/peers` | `handleMeshListPeers` |
| POST | `/v1/mesh/peers` | `handleMeshAcceptPeer` |
| DELETE | `/v1/mesh/peers/{name}` | `handleMeshRemovePeer` |
| GET | `/v1/mesh/self` | `handleMeshSelf` |
| GET | `/v1/mesh/settings` | `handleGetMeshSettings` |
| PUT | `/v1/mesh/settings` | `handlePutMeshSettings` |
| GET | `/v1/mesh/tokens` | `handleMeshListTokens` |
| DELETE | `/v1/mesh/tokens/{tokenID}` | `handleMeshRevokeToken` |
| GET | `/v1/plugins` | `pluginHandler.HandleListPlugins` |
| GET | `/v1/plugins/catalog` | `pluginHandler.HandleListPluginCatalog` |
| GET | `/v1/plugins/channels` | `pluginHandler.HandleListPluginChannels` |
| PUT | `/v1/plugins/channels` | `pluginHandler.HandleSetPluginChannel` |
| POST | `/v1/plugins/create` | `pluginHandler.HandleCreatePlugin` |
| POST | `/v1/plugins/install` | `pluginHandler.HandleInstallPlugin` |
| GET | `/v1/plugins/marketplaces` | `pluginHandler.HandleListMarketplaces` |
| POST | `/v1/plugins/marketplaces` | `pluginHandler.HandleAddMarketplace` |
| POST | `/v1/plugins/marketplaces/sync` | `pluginHandler.HandleSyncMarketplaces` |
| DELETE | `/v1/plugins/marketplaces/{id}` | `pluginHandler.HandleDeleteMarketplace` |
| POST | `/v1/plugins/prune` | `pluginHandler.HandlePrunePlugins` |
| POST | `/v1/plugins/sync` | `pluginHandler.HandleSyncMarketplaces` |
| DELETE | `/v1/plugins/{catalogID}` | `pluginHandler.HandleUninstallPlugin` |
| PUT | `/v1/plugins/{id}` | `pluginHandler.HandleUpdatePlugin` |
| GET | `/v1/plugins/{id}/apps` | `pluginHandler.HandleListPluginApps` |
| PUT | `/v1/plugins/{id}/apps/{alias}` | `pluginHandler.HandleBindPluginApp` |
| GET | `/v1/plugins/{id}/config` | `pluginHandler.HandleGetPluginConfig` |
| PUT | `/v1/plugins/{id}/config` | `pluginHandler.HandleUpdatePluginConfig` |
| GET | `/v1/plugins/{id}/dependencies` | `pluginHandler.HandleListPluginDependencies` |
| POST | `/v1/plugins/{id}/mcp/{serverName}/toggle` | `pluginHandler.HandleTogglePluginMCP` |
| POST | `/v1/plugins/{id}/upgrade` | `pluginHandler.HandleUpgradePlugin` |
| GET | `/v1/preferences` | `sysadminHandler.HandleGetPreferences` |
| PUT | `/v1/preferences/{key}` | `sysadminHandler.HandleSetPreference` |
| GET | `/v1/projects` | `chatHandler.HandleListProjects` |
| POST | `/v1/projects` | `chatHandler.HandleCreateProject` |
| DELETE | `/v1/projects/{id}` | `chatHandler.HandleDeleteProject` |
| GET | `/v1/projects/{id}` | `chatHandler.HandleGetProject` |
| PUT | `/v1/projects/{id}` | `chatHandler.HandleUpdateProject` |
| GET | `/v1/providers` | `providerHandler.HandleListProviders` |
| POST | `/v1/providers` | `providerHandler.HandleCreateProvider` |
| POST | `/v1/providers/from-catalog` | `providerHandler.HandleCreateProviderFromCatalog` |
| DELETE | `/v1/providers/{providerID}` | `providerHandler.HandleDeleteProvider` |
| PUT | `/v1/providers/{providerID}` | `providerHandler.HandleUpdateProvider` |
| GET | `/v1/providers/{providerID}/models` | `providerHandler.HandleListModels` |
| POST | `/v1/providers/{providerID}/models` | `providerHandler.HandleCreateModel` |
| DELETE | `/v1/providers/{providerID}/models/{modelID}` | `providerHandler.HandleDeleteModel` |
| PUT | `/v1/providers/{providerID}/models/{modelID}` | `providerHandler.HandleUpdateModel` |
| POST | `/v1/providers/{providerID}/models/{modelID}/deprecate` | `providerHandler.HandleModelDeprecate` |
| POST | `/v1/providers/{providerID}/models/{modelID}/upgrade` | `providerHandler.HandleModelUpgrade` |
| POST | `/v1/providers/{providerID}/test` | `providerHandler.HandleTestProvider` |
| GET | `/v1/search` | `chatHandler.HandleSearch` |
| GET | `/v1/sessions` | `chatHandler.HandleListSessions` |
| DELETE | `/v1/sessions/{sessionID}` | `chatHandler.HandleDeleteSession` |
| GET | `/v1/sessions/{sessionID}` | `chatHandler.HandleGetSession` |
| GET | `/v1/sessions/{sessionID}/context` | `chatHandler.HandleGetSessionContext` |
| PUT | `/v1/sessions/{sessionID}/project` | `chatHandler.HandleMoveSession` |
| POST | `/v1/sessions/{sessionID}/recap` | `chatHandler.HandleSessionRecap` |
| GET | `/v1/skills` | `sysadminHandler.HandleListSkills` |
| GET | `/v1/skills/commands` | `chatHandler.HandleListSkillCommands` |
| POST | `/v1/skills/create` | `handleSkillCreate` |
| GET | `/v1/skills/injections` | `pluginHandler.HandleListSkillInjections` |
| POST | `/v1/skills/injections/trust` | `pluginHandler.HandleTrustSkillInjection` |
| POST | `/v1/skills/install` | `sysadminHandler.HandleInstallSkill` |
| GET | `/v1/status` | `handleStatus` |
| GET | `/v1/system/capabilities` | `handleGetCapabilities` |
| POST | `/v1/system/update` | `sysadminHandler.HandleTriggerUpdate` |
| GET | `/v1/system/version` | `sysadminHandler.HandleGetVersion` |
| GET | `/v1/tools` | `sysadminHandler.HandleListTools` |
| POST | `/v1/tools/{name}/execute` | `sysadminHandler.HandleExecuteTool` |
| GET | `/v1/usage` | `sysadminHandler.HandleGetUsage` |
| POST | `/v1/vault/rotate-master-key` | `sysadminHandler.HandleVaultRotateMasterKey` |
| GET | `/v1/webhooks/{channelType}/{channelID}` | `sysadminHandler.Channels.HandleWebhookReceive` |
| POST | `/v1/webhooks/{channelType}/{channelID}` | `sysadminHandler.Channels.HandleWebhookReceive` |
| GET | `/v1/workflows` | `sysadminHandler.Workflow.HandleListWorkflows` |
| POST | `/v1/workflows` | `sysadminHandler.Workflow.HandleCreateWorkflow` |
| GET | `/v1/workflows/runs/{id}/data/{step_key}` | `sysadminHandler.Workflow.HandleGetRunStepData` |
| POST | `/v1/workflows/runs/{id}/resume` | `sysadminHandler.Workflow.HandleResumeWorkflowRun` |
| DELETE | `/v1/workflows/{id}` | `sysadminHandler.Workflow.HandleDeleteWorkflow` |
| GET | `/v1/workflows/{id}` | `sysadminHandler.Workflow.HandleGetWorkflow` |
| PUT | `/v1/workflows/{id}` | `sysadminHandler.Workflow.HandleUpdateWorkflow` |
| GET | `/v1/workflows/{id}/runs` | `sysadminHandler.Workflow.HandleListWorkflowRuns` |
| POST | `/v1/workflows/{id}/trigger` | `sysadminHandler.Workflow.HandleTriggerWorkflow` |
| POST | `/v1/workspace/upload` | `sysadminHandler.HandleVFSUpload` |

共 163 条，提取自 `internal/gateway/server/server_routes.go`（`mux.HandleFunc`/`mux.Handle` 全量扫描，不含 `server_init.go` 里的静态资源兜底路由）。本表是代码事实的权威快照，供与上方 §1.2 手写分组罗列交叉核对——手写罗列携带跨小节引用与语义分组，不由本表自动替换。
<!-- END GENERATED: m13-route-inventory -->

### 1.3 WebSocket [计划：可选升级路径]

当前推送走 SSE，client→server 走 REST。升级设计约束（实现时以此为规范）：
- 读 goroutine: ClientMessage JSON→Intent→Agent.Input
- 写队列 cap=256：Critical 事件（tool_call/error/approval/complete）不可丢弃；Streaming（token/thinking）可合并
- Critical 满→合并 Streaming 腾 slot；无可合并→Force Disconnect + [EventLog] 回放；严禁 drop-oldest
- `polaris_ws_coalesced_events` Counter 监控

### 1.4 Web UI

实现规约见 §8。`ServeWebUI`：`DEV_MODE=1` → 反向代理 Vite dev server（`:5173`）；生产 → `http.FileServer(http.FS(subFS))` 挂载 `go:embed all:dist`。`FeatureGate.FeatureWebUI` 控制是否注册 `/` 路由，关闭时为 API-only 模式（CLI 不受影响，M03 §5）。

### 1.5 Rate Limiting

**目标设计（RateLimiterMiddleware）**:
- Token Bucket GCRA，双层隔离（进程指纹 + client_type 复合键）；fingerprint：本地 → PID+启动时间 hash，远程 → M11 Ed25519 AgentIdentity 公钥 hash
- 熔断：连续 3 个 1s 窗口 >100% 配额 → 隔离 30s（429 + Retry-After:30）
- 配额（per fingerprint+client_type）：CLI 50/s；WebUI 30/s；A2A 30/s；WS 5/s；`/_admin/` 10/s

**现状**：`cmd/polaris/boot_server.go` 注入单桶 `rate.NewLimiter(50, 100)`，`internal/gateway/server/middleware.go` 的 `RateLimiter` 为单桶 Token Bucket；指纹分层/熔断/分 client_type 配额未实现。

---

## 2. 对内调度

### 2.0 Resource Governor

`internal/automation/resource_governor.go`（`ResourceGovernor`）；**阈值 SSoT: `spec/state.yaml §thresholds.memory_pressure`**，与 M3 OSMemoryGuard 共享同一配置节，禁止独立硬编码。字段：`maxConcurrent` / `inFlight(atomic.Int32)` / `cpuThreshold(70%)` / `memThresholdMB(1024MB=L2 紧急快速拒绝门限，[Tier-0-Limit])`。

**准入语义**:
1. `priority=0`（交互）始终放行，但有 `maxConcurrent×4` 硬上限（防 OOM）。
2. CPU>70% 或空闲<1024MB → 拒绝非用户交互。
3. >50MB 双层校验：经注入的 `memProbeFn()` 自维护 `MemL1FreeMB`/`MemL2FreeMB`/`MemL3FreeMB` 三级阈值做独立状态机判断（不直调 `OSMemoryGuard.CurrentPressureLevel()`）；L1 正常且瞬时>512MB → 放行；L2 → 仅 `priority<=1`；L3 → 拒绝所有非用户交互。
4. `inFlight>=maxConcurrent` → 优先级降序抢占。
5. Release：`inFlight--`；空闲>`memThresholdMB+256MB` 回滞 → `Cond.Broadcast()`。

**降级（三级，与 M3 共享阈值）**:
- **L1 预警**：空闲 <1.5GB 或 CPU >70% → 拒绝 `priority>=3`
- **L2 紧急**：空闲 <1.0GB 或 CPU >85% → 拒绝 `priority>=2`
- **L3 临界**：空闲 <512MB 或 OOM → 拒绝 `priority>=1` + 通知 M3

**入口与探针**（以代码为准）：`Admit` 全仓无调用方；后台工作走 `AdmitBackground(work)`（不排队、不打活跃标记，内存低于 L2 或 CPU 超 L1 即拒；boot 层入口 `cmd/polaris/boot_substrate.go:admitBackground`）。LLM 并发走 `AdmitLLM` / `WaitForLLMCapacity`（上限 `spec/state.yaml` `MaxConcurrentLLMCalls`，调用方 `InferenceRouter`）：priority 0 = 用户可见推理，只受并发上限约束且唯一刷新活跃时间；1 = 后台推理（`protocol.WithBackgroundWork` 标记：空闲自进化、`AcquireHeadless`），另受水位线约束，被拒时 `InferenceRouter` 按 2s 间隔挂起重试至 ctx 到期。CPU/内存探针归口 `internal/observability/probe`（`CPUSampler`：linux `/proc/stat` 增量、darwin `vm.loadavg`÷核数、windows `GetSystemTimes`；linux 容器内内存按 cgroup working set）。未引入独立 GlobalSemaphore，复用 ResourceGovernor。

**local_only 死锁恢复**：`local_only` 下 OSMemoryGuard 卸载本地 LLM → 任务堆积占内存 → 无法重载 LLM（循环死锁）。当 (a)ErrLocalModelUnavailable>30s + (b)压力>=L2 + (c)待处理>0 时，通知 M8 将 Priority>=2 任务回退 Suspended(oom_evicted)；每释放 256MB 重试重载；仍失败 → HITL 手动介入。本地推理默认关闭（ADR-0109），该恢复仅在用户显式启用本地推理时适用。

### 2.1 任务队列与全局并发信号量

**TaskStore**：Pub/Sub，接口 `Enqueue/MarkComplete/MarkFailed/ListPending/Subscribe(<-chan TaskEvent)/Close`；实现 `SQLiteScheduler`（`internal/automation/queue.go`），事件驱动而非轮询，`[Tier-0-Limit]` 非热路径。类型定义：`internal/automation/`（ResourceGovernor / TaskQueue）、`internal/automation/hitl/`（HITL）。

**交付语义 At-Least-Once**：`SQLiteScheduler.Start(ctx, dispatchFn)` 启后台扫描 goroutine，每 5s 扫 `scheduler:task:` 前缀，CAS 更新 Status: pending → running → completed/failed；崩溃重启后自动重试直至 MaxAttempts；幂等键 = `Task.IdempotencyKey`。

**Agent 任务直通**：`task.Type == "agent"` 时扫描循环优先调 `protocol.AgentInvoker.InvokeAgent(ctx, string(task.Payload))`，经 `SetAgentInvoker` 注入（`boot_agent.go` 绑定 `agentInvokerAdapter`）；未注入或非 agent 类型回落 `dispatchFn`。

**终态通知投递**：`notifyTaskTerminal` 在任务进入终态（重试耗尽失败 / 成功完成；`Pool=="intent_handler"` 交互任务除外）时向 `protocol.OutboxWriter` 写 `protocol.TopicNotification="notification"` 事件；消费端 `internal/automation/notify.Dispatcher` 复用 `internal/store/outbox_worker.go` 的 `OutboxWorker`（指数退避重试），从 `PreferenceReader`（`016_preferences` 表）读 Webhook URL 并 POST；payload 畸形/未配置 Webhook 为软跳过（不重试），非 2xx 或网络错误返回 error 交 OutboxWorker 重试。当前仅 Webhook 一种渠道。

### 2.2 定时任务

`internal/automation/cron.go`：5 字段标准 Cron 解析器（分 时 日 月 周；`*`/数字/范围/步进/列表），解析错误返回 `apperr.CodeInternal`。automation 触发见 §8.7（`cronTick`），月度成本报告见 §1.1。

**设计清单（代码库无统一 `Scheduler.Start`，未按此形态实现）**：每 1h 记忆归并 `RunConsolidation` / 每天 02:00 `evalRunner.RunNightly` / 每 5min `killSwitch.CheckAndAct([TokenBurnRate])` / 每周日 03:00 `CheckEmbeddingDrift` / 每 30min（空闲）`selfImprove.IdleLoop`。

### 2.3 Resource Reaper

**设计（`ResourceReaper` 类型当前代码库不存在，清理职责分散在各存储模块）**：注册 `"0 4 * * *"`，`minInterval(24h)`，前置 `isDeepIdle()`；超 6:00 未获空闲 → M9 SuspendAll → 清理 → ResumeAll → M11 Audit。步骤：
1. PruneDeprecatedSkills：30 天未检索 + 成功率<30% → 删除 Wasm
2. PruneOrphanEntities：无入/出边 + 90 天未更新
3. PruneWorkspaceFiles：>7 天且关联 `Task Status∈{Done,Failed}` → `os.RemoveAll(workspace/<task_id>/)`；按 CreatedAt 升序回收至 < maxSize×0.7；紧急模式（写入拦截触发）→ 同步 RunNow
4. CompressColdArchive：>180 天 JSONL → zstd
5. storageFabric.Vacuum / CompactSurrealDB / RebuildStaleIndexes

### 2.4 HITL ([ESCALATE])

**HITLGateway**：`internal/automation/hitl/`（`GatewayImpl`）；Notifier 接口 `NotifyApproval(req)` / `NotifyResolved(req,outcome)`，实现 SlackNotifier(Webhook) / EmailNotifier(SMTP)。重试 3 次（100ms→500ms→2s），失败回退 Slack→Email→本地（chat:stderr+BEL；serve:syslog CRITICAL + Web UI `/_admin/alerts`）；上限 min(HITL timeout×10%, 2min)，确定性失败不重试。

**HITLStore**：`hitl_requests` 表（`044_hitl_requests.sql`，ADR-0104 决策五），经消费端接口 `RequestStore` 读写，实现 `internal/store/repo.SQLiteHITLRequestRepository`。一行一审批：`status ∈ pending|approved|denied|timeout|orphaned`；`decided_by ∈ human|auto_approve|auto_deny|trust_downgrade|timeout_kill|l3_gate`。`Respond` 用 `UPDATE … WHERE id=? AND status='pending'` 条件更新（未命中即拒绝，二次裁决幂等）；kill_pause 超时落 `timeout`；信任降级放行与 L3 门禁 P0 自动拒绝直接落终态行；启动期孤儿对账把遗留 pending 置 `orphaned`（`cmd/polaris/boot_orphan_reconciler.go`）。内存 waiters map 把裁决分发给阻塞中的 `Prompt`（channel 随进程消失，重启后 pending 必为孤儿）。

**TrustScorer**：无内存状态，`ShouldDowngrade` 查 `hitl_requests`——同 `(checkpoint_type, agent_id)`、`decided_by='human'`、`decided_at` 在 Window 内且晚于最近一次人工 denied 的 approved 行数 ≥ MinApprovals；硬地板（TaintLevel≥Medium / RiskLevel≥3 / 设备操控 / l4_multi_sig / 无 AgentID）不参与；查表失败不降级（fail-closed）。

**ApprovalRequest**:
- `ID / AgentID / Action / Detail / RiskLevel / CreatedAt / Timeout`（默认值见 `spec/state.yaml §m13_interface.hitl_default_deadline_minutes_normal`，紧急/长程档见同节 `_urgent` / `_long`，Per-TaskType 可覆盖）
- `TimeoutPolicy`: `kill_pause`(默认) / `auto_deny` / `auto_approve`(policy_version>=2 + 管理员授权)
- `Status`: `pending / approved / denied / timeout`

**auto_approve 硬编码约束**（`internal/config/immutable_constants.go`，CI 不可变内核）:
- **禁止**: `write_network, privileged, delete_data, execute_system, modify_policy`
- **白名单**: `read_local_file, log_rotate, cache_evict, stats_collect`
- **[Taint-Medium] 感知**: `ActiveContext.TaintLevel >= [Taint-Medium]` → auto_approve 失效，升级 HITL
- **敏感路径 glob**: `**/.env*, **/*id_rsa*, **/*.pem, **/credentials*, **/*.key, **/secret*, **/.ssh/**, **/.aws/**, **/.gcloud/**, **/kubeconfig*`
- **Symlink 防御**: `filepath.EvalSymlinks(filepath.Abs(filepath.Clean(path)))` 先于 glob
- **原子 etag 校验**: auto_approve 放行到 JIT Token 签发为临界区——签发前原子比对当前 `[Cedar-Gate]` policy_version/etag 与决策时刻的 decision_etag；不一致 → 决策上下文过期，拒绝 auto_approve 并升级 HITL（防 Cedar 热更新使刚签发的 Token 被 M7 L3PolicyMonitor 掐断，M7 §4.7）

**RequestApproval**: ① 加入 pending ② 持久化 ③ Notifier 发送 ④ `[EventLog]` Subscribe ApprovalResolved ⑤ 等待：收到 → nil/ErrApprovalDenied；超时 → 用户交互 `ErrApprovalTimeout`+S_ROLLBACK（不触发 `[KillSwitch]`），后台 auto_deny。
**ResolveApproval**: ① pending 查找 → Approved/Denied+Comment ② 持久化+审计 ③ `[MutationBus]` → `[EventLog]`。
**ReloadOnStartup**: 加载 pending → 超时检查 → 超时则 ApprovalTimeout；未超时则重启计时器 + 重通知（dedup: SHA-256(requestID+retry_seq)，>50 条 → 仅最近 50 + M3 CRITICAL）。

**权限模式联动**（inv_M13_07）：`ApprovalRequest.PermissionMode` 仅电脑/浏览器操控类 checkpoint（`CheckpointDeviceControlReview`，由 `interceptComputerUse` 发起）填充，取值对应「设置 → 设备操控 → 权限模式」（`default`/`auto_review`/`full_access`）：
- `full_access`：`interceptComputerUse` 的 `needHITL` 恒 `false`，正常不发起审批；`resolveTimeoutAction` 的 `full_access → auto_approve` 是防御性兜底，保证即使仍发起审批且无人应答，行为也与"自主执行"承诺一致，而非卡死。
- `auto_review`/`default`：走通用兜底 `kill_pause`——高危操作需人审，超时不放行是设计如此。
- 该联动**只作用于电脑操控类 checkpoint**，不影响扩展安装审查（`security_review`，TrustTier + Cedar `install_extension_permit`）、L3/L4 自我改进晋升（`l3_strategy_modify`/`l4_multi_sig`）、自动化预执行（`automation_pre_run`）——二者是独立的两个轴，不得混淆。
- `TaintLevel >= TaintMedium` 的硬拒绝地板优先于权限模式判断，防止被污染/被提示注入的 Agent 借用户的设备操控设置绕过对外部不可信内容的强制复核。

**咨询式 HITL / Generative UI（提案，未实现）**：`AskHuman` 特权工具复用 `HITLPrompt`/`HITLResponse`（新增 `CheckpointType=clarification_request`），不新建平行网关路径 → ADR-0066 决策七（原 ADR-0042）。

### 2.5 TrafficSplitter

**设计（`TrafficSplitter` 类型当前代码库不存在）**：`baseline(string) / candidate(string) / percent(int32, 原子读写)`。
- **Route**: percent<=0 → baseline；>=100 → candidate；否则 `fnv32(sessionID)%100 < percent` → candidate
- **SetPercent**: clamp 0–100 后 atomic.Store；**Rollback**: atomic.Store 0（仅重置，无内置告警）
- **分工**: M9 决策+回滚检测；M13 执行；M12 对比

---

## 3. MCP

**MCP 客户端**（Polaris 连接外部 MCP Server）：`internal/extension/mcp/`（`MCPManager`，`StartFromDB(serverID)` 以 `mcp_servers` 行为配置权威源），传输与污点解码见 ADR-0103 决策十一，2026-07-28 双纪元协议见 ADR-0103 决策八与 M13-bis §8.5；工具注册 `Tool{Name, Source=SourceMCP, SourceURI=server.Name}` → ToolRegistry。管理端点 `/v1/mcp-servers`（§1.2）。

**MCP 服务端**（Polaris 作为 MCP Server 对外）：原 stdio `StartMCPServer`（execute_skill / search_knowledge / memory://episodic/recent）当前代码库无实现；对外 MCP 面现为 Polaris 间 MCP 门面 `/mcp`（`internal/gateway/server/meshapi/mcp.go`，ADR-0111 决策二）。

---

## 6. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---------|---------|---------|
| HTTP server 阻塞/超载 | 限流 (429+Retry-After) + 504 超时 | 负载降低后恢复 |
| SSE 连接断开 | 客户端 EventSource 自动重连（指数退避） | — |
| TaskQueue SurrealDB-Core 持久化失败 | 降级纯内存队列 (at-most-once) + CRITICAL 告警 | 恢复后切回 at-least-once |
| Cron 定时器错失 | `cronTick` 下一轮按 `next_run_at <= NOW()` 补触发一次 | — |
| HITL Notifier 全部通道失败 | 本地回退 (chat:stderr+BEL / serve:syslog CRITICAL) | — |
| ResourceGovernor 拒绝新任务 (L3 临界) | 503 + Retry-After | 内存恢复后 Admit 放行 |

与 OSMemoryGuard 协同：ResourceGovernor 实时读取内存压力 → 准入；L1/L2/L3 阈值同源（00-Global-Dictionary §1-ter XR-07）。

## 6-bis. 已知 Bug 修复记录

| # | 级别 | 位置 | 问题 → 修复 |
|---|------|------|------------|
| 1 | P3 | `handleGetSession` | `TaskDuration` 的 time.Parse layout 为 `"2006-01-02T15:04:05Z"`，SQLite `datetime()` 实际输出空格分隔无 Z，两次 Parse 静默失败使其恒为 0 → 改用兼容两种格式的 `parseDBTime` |
| 2 | P1 | `executeAutomation` | `result_action="channel:XXX"` 调 `SendReply(channelType="")` 走 `default` 分支被静默丢弃 → 先查 `channels` 表取 `type`+`config_json` 再调用 |
| 3 | P3 | `generateCostReport` | 1 月时 `int(now.Month())-1=0` 输出 `"YYYY-00"` → 由 `monthStart` 派生，1 月正确输出上年 `"YYYY-12"` |

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m13_interface`。

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
| 接口定义 | SafeDialer/Blackboard/TaskEntry/ScheduledTask | internal/protocol/interfaces_*.go, types.go |
| 全局字典 | ESCALATE/KillSwitch/SSRFGuard 定义 | 00-Global-Dictionary §3, §4 |
| 时序图 | KillSwitch 触发链（M13 SealedMiddleware 503 响应）| DIAGRAMS.md#killswitch |

---

## 8. Web UI 规约

> 前端实现的单一权威源。栈: Alpine.js + Tailwind CSS v4 + DaisyUI + Vite 6 + go:embed + marked | [FeatureGate.FeatureWebUI] | [Tier-0-Limit]。桌面版加载同一套 UI（§1.1.1）。

### 8.1 目录结构

```
web/
├── embed.go                  # go:embed all:dist → WebUIFS embed.FS
├── vite-plugin-fragments.js  # 构建时展开 <page-fragment src="…"> 为内联 HTML
├── src/
│   ├── index.html            # 壳页（侧边栏 + page-fragment + 浮动日志抽屉）
│   ├── pages/                # chat sessions projects search eval monitor settings plugins skills automation + onboard
│   ├── js/
│   │   ├── app.js            # Alpine stores 入口 + marked 配置 + URL 路由初始化（legacyPageMap）
│   │   ├── sse.js            # SSEClient（fetch+ReadableStream + 指数退避重连）
│   │   ├── utils.js / i18n.js
│   │   └── store/            # 按域拆分的 Alpine 状态（chat/logs/nav/statusBar/projects/mesh/audio/computer/…）
│   └── css/style.css         # Tailwind v4 入口 + 主题色 token
├── package.json
└── vite.config.js
dist/                         # Vite 输出（gitignore；make build-ui 生成）
```

### 8.2 页面结构与导航

侧边栏入口，聚合页以 Tab 内嵌，日志为浮动抽屉：

| 导航入口 | 路由 | 子 Tab / 说明 | 主要 API |
|----------|------|--------------|---------|
| 新对话 | `/` | 权限模式下拉内联于输入区 | `POST /v1/agent/stream` (SSE) |
| 项目 | `/projects` | 项目管理（ADR-0097） | `/v1/projects` |
| 会话 | `/sessions` | — | `GET /v1/sessions` |
| 搜索 | `/search` | — | `GET /v1/search` |
| 插件 | `/plugins` | 插件 · 应用 · MCP · 技能 · 市场（五合一） | `/v1/plugins/*` |
| 技能 | `/skills` | 已安装技能（独立） | `GET /v1/skills` |
| 自动化 | `/automation` | 定时自动化 · 工作流 · 触发器(Webhook) · 待办审批(HITL) | `/v1/automations` · `/v1/workflows` · `/v1/approvals/*` |
| 监控 | `/monitor` | 状态 · 洞察 · Agent | `/v1/status` 轮询 · `/v1/insights` |
| 设置 | `/settings` | 提供方 · 接入 · 配置 · 电脑操控 · 多节点 | `/v1/providers` · `/v1/channels` · `/v1/config` · `/v1/preferences` · `/v1/mesh/*`（ADR-0111） |
| 评测 | `/eval` | — | `/v1/eval/*` |
| **日志抽屉** | FAB 唤出，不占导航位 | 浮动侧滑面板 | `GET /v1/logs/stream` (EventSource) |

- 旧路由兼容：`app.js` 的 `legacyPageMap` 在 DOMContentLoaded 将旧 URL（status/insights/providers/channels 等）重定向至聚合页，无需服务端 redirect。
- **Tab 懒加载**：子 Tab 首次激活才加载数据，页级 `x-data` boolean flag 去重（`inv_webui_07`）；例外：Providers/ModelRoles 进入 settings 时预加载（首屏需要）。

### 8.3 核心协议与渲染

> UI 硬规则集中在 §8.5 `inv_webui_NN` 表（单一 grep 源）；下方 `(inv_*)` 为交叉引用。

#### 8.3.1 Chat SSE 渲染

- **流端点**: `POST /v1/agent/stream`；因需 POST body 用 `fetch+ReadableStream`（`sse.js:SSEClient`），非原生 `EventSource`。
- **状态机** `(inv_webui_08)`: `IDLE → SUBMITTING → THINKING → STREAMING → TOOL_RUNNING → STREAMING → COMPLETE → IDLE`。
- **工具生命周期**：`TOOL_RUNNING` 期间 SSE 额外推 `tool_start` / `tool_progress` / `tool_success` / `tool_error`，支持中间态渲染。
- **异常退避**: 1s→2s→4s→8s→16s→30s，超 10 次转 ERROR。
- **中断恢复**: 收到 `error(interrupted)`，保留 `currentTokens` 并打橙色"⚠ 已中断"徽章，消息持久化入列。
- **幂等去重** `(inv_webui_10)`: `dedupeRunID(sessionID, input)` 5s 窗口内同元组返回同一 runID。

#### 8.3.2 文本规范化与 Markdown

- **安全沙箱** `(inv_webui_09)`: `marked` 渲染，白名单过滤 HTML（清 `script`、`on*`、`javascript:`），`<a>` 强制 `rel="noopener noreferrer"`。
- **静默过滤**: `sanitizeContent()` 剥除 XML(`<tool_call>`)、`[[reply_to_*]]`、`NO_REPLY`。
- **思维剥离**: `event:thinking` 仅入 `ThinkingPanel`，不混入用户气泡。

#### 8.3.3 日志 SSE 流（浮动抽屉模式）

原生 `EventSource`（GET）连 `GET /v1/logs/stream`；连接生命周期绑定抽屉开关 `(inv_webui_06)`（`openDrawer()`→`connect()`，`closeDrawer()`→`disconnect()`，避免后台常驻）；500 条环形缓冲（`ringCap=500`）；3s 固定间隔断线重连（无退避）；级别过滤经 `levelFilter` 重建连接（`setLevel()`→`connect()`）。

#### 8.3.4 Generative UI（提案，未实现）

新增 SSE 事件 `ui_component`；渲染工具（`render_chart`/`render_form`）经 `protocol.ToolRegistry.ExecuteTool` 注册（`SandboxTier=InProcess`），先写 `events` 表后广播；前端按组件白名单渲染，禁止 `x-html` 挂载原生 HTML。→ ADR-0066 决策七（原 ADR-0042 决策二/ADR-0043）。

---

### 8.4 UI 组件与交互

| 组件/交互 | 触发/机制/结果 |
|----------|--------------|
| **ToolExecutionAccordion** | 顶部显示工具总耗时（`created_at`/`updated_at` 差值）；折叠态（"正在使用 Browser 搜索..."）/ 展开态（内嵌 Markdown/Terminal，**高亮实际执行命令**）；创建/修改文件的操作可展开完整内容与 Diff；结束态成功绿勾 / 失败红叉+重试 |
| **StatusBar** | 10s 轮询 `/v1/status`；Token 烧率 >80% 橙色徽章，>95% 红色告警并提醒将压缩；语音资产就绪进度由单一 chip 汇总 |
| **ApprovalCard** | 风险分级色阶（蓝/橙/红），闪烁倒计时；任务导航项显示待审角标 |
| **CompactionDivider** | 压缩检查点 `at_message_id` 注入占位块，支持从此分支发起新会话 |
| **OnboardWizard** | 首次引导，DOMContentLoaded 后 400ms 延迟检查；Provider/Model/Channel 三步 |
| **浮动日志抽屉** | 右下角 FAB 唤出；打开建 EventSource，关闭断开 `(inv_webui_06)`；未读日志 FAB 显绿点 |
| **Tab 内嵌导航** | `.tab-bar`；面板用 `x-show`（非 `x-if`）保留 DOM 状态 `(inv_webui_11)` |
| **键盘快捷键** | `Enter` 提交，`Shift+Enter` 换行，`Ctrl+C` 中断流，`/` 唤出斜杠补全 |
| **主题 / 语言** | `--color-surface` 变量系（Tailwind `@theme`），system/dark/light/terminal，持久化 localStorage；`$store.i18n.setLang('zh'\|'en')`，数据集中 `js/i18n.js` |
| **语音输入 (STT)** | 录音（WebM/MP4）`multipart/form-data` 提交 `POST /v1/audio/transcriptions`，触发 `stt-result` 回填输入框。后端本地 Sherpa-ONNX + SenseVoice（零 Python 依赖，满足 Tier 0）。资产由守护进程就绪 45s 后的单一预置器后台串行下载（`inference.audio.auto_install=true` 默认开；可 `POST /v1/audio/stt/install` 手动触发），失败 5m/30m/2h 退避重试；未就绪返回 503 JSON `{"error":"stt_not_ready","state","detail","message"}`；状态经 `GET /v1/audio/status` 与 `GET /v1/system/capabilities` 暴露。模型/ABI/状态机细节 → ADR-0110（决策一/六/七/八） |
| **语音合成 (TTS)** | `POST /v1/audio/speech` `{"input":"..."}` → 音频字节流，`Content-Type` 取 Provider MIME（缺省 `audio/wav`）；未就绪 503 JSON `{error,state,detail,message}`；手动安装 `POST /v1/audio/tts/install`。服务端仅 MeloTTS zh_en（可选 `provider="http"` sidecar），Edge/Piper/Kokoro/Matcha 已移除。**硬件三档**：A（总内存 <1800MB 或 <2 核）STT/TTS 全关；B（<3600MB）仅 STT；C（≥3600MB 且 ≥2 核）STT + MeloTTS。A 档、B 档（原因码 `tts_ram_tier`）、基准过慢或准备中时前端退回 `speechSynthesis`（`inference.tts.engine=auto\|server\|system`）；`tts_status.model` 为 melo\|none。基准门控/抗争用/代理测速/空闲卸载 → ADR-0110 决策三~六 |

---

### 8.5 构建与不变量

**构建/运行**: 生产 `make build-ui`（`npm install && npm run build` → `web/dist/`）；开发 `make dev-ui`（Vite `:5173`，代理 `/v1` 至 `:28888`）；`make build` 自动先调 `make build-ui`。

**[Tier-0] 核心不变量**（UI 硬规则单一 grep 源）:

| ID | 约束 | 验证位置 |
|----|------|---------|
| `inv_webui_01` | `dist/` 不入 Git；`make build` 必先调 `make build-ui`。 | 根 `.gitignore`（`web/dist/*`）+ `Makefile` |
| `inv_webui_02` | npm `dependencies` 仅 `alpinejs` + `marked`；`devDependencies` 仅 `@tailwindcss/vite` + `daisyui` + `vite`；零 CDN 依赖（内网离线可用）。 | `web/package.json` |
| `inv_webui_03` | `FeatureGate.FeatureWebUI=false` 或密封(`SEALED`)态时，API 拒服，UI 出强警告横幅。 | `internal/observability/` |
| `inv_webui_04` | 写操作携带 `X-Session-Token`（`sessionStorage.getItem('polaris_token')`）。 | `web/src/js/sse.js` + middleware |
| `inv_webui_05` | 轮询故障禁止静默（连续 3 次挂 Warning Banner）。 | `web/src/js/store/statusBar.js` |
| `inv_webui_06` | 日志 SSE 连接不得常驻后台，必须随抽屉关闭 `disconnect()`。 | `web/src/js/store/logs.js` |
| `inv_webui_07` | Tab 聚合页内每个子 Tab 的数据加载至多触发一次（lazy flag 防重）。 | `web/src/pages/` 各页 x-data lazy flags |
| `inv_webui_08` | Chat SSE 状态机仅在 `IDLE → SUBMITTING → THINKING → STREAMING → TOOL_RUNNING → STREAMING → COMPLETE → IDLE` 路径迁移；禁跳态。 | `web/src/js/store/chat.js` |
| `inv_webui_09` | `marked` 渲染必经白名单过滤：清 `script` / `on*` 属性 / `javascript:` URI；`<a>` 强制 `rel="noopener noreferrer"`。零例外。 | `web/src/js/app.js` marked 配置 |
| `inv_webui_10` | `dedupeRunID(sessionID, input)` 5s 窗口对相同元组返回同一 runID。 | `web/src/js/store/chat.js:dedupeRunID` |
| `inv_webui_11` | Tab 内嵌导航必用 `x-show`（非 `x-if`），防 `setInterval` 在切换时重置。 | `web/src/pages/monitor.html` `settings.html` `automation.html` |

---

## 8.6 插件聚合市场（`/plugins` 页）

扩展体系（分类、信任模型、安装实例表）→ ADR-0103；安装/卸载/市场同步全流程与表关系 → M13-bis §1/§5/§12。本节只留 UI 契约。

### DB 表

| 表 | 用途 | 迁移 |
|----|------|------|
| `plugin_marketplaces` | 市场源订阅 | 018 |
| `extension_catalog` | 市场同步后的目录缓存（只读，不驱动执行） | 019 |
| `extension_instances` | 已安装扩展单一事实来源（SSoT） | 020 |
| `plugins` | 已安装 Plugin 运行时记录（manifest 快照） | 021 |
| `skills` / `mcp_servers` | Skill / MCP 运行参数（可带 `plugin_id` FK） | 008 / 015 |

`app` 扩展类型与 `apps` 表已删除（ADR-0103 决策四，Codex 应用 = 插件内连接器绑定 `plugin_app_bindings`）。

### 业务流

- **安装** `POST /v1/plugins/install`：`{ catalog_id, type }` → `Manager.InstallExtension`（授权 + 写 `extension_instances`）→ 按类型安装器写 `mcp_servers / skills / plugins` 并热注册 ToolRegistry（按 `trust_tier` 选 InProcess 或 Wasm 沙箱）。卸载 `DELETE /v1/plugins/{catalogID}` → 彻底卸载流程（M13-bis §5.7）。
- **市场同步** `POST /v1/plugins/sync`：后台 goroutine 遍历 `enabled` 的 `plugin_marketplaces`，经 M11 SafeDialer 拉取并解析，UPSERT `extension_catalog`，立即 202；客户端轮询 `GET /v1/plugins/catalog` 感知完成。
- **安全门控**：TrustTier 五级与各级安装/运行时策略见 M13-bis §6；`trust_tier` 安装时强制从 `extension_catalog` 继承，禁止客户端覆盖。

### 展现层 SSoT 分离（前端监控页 / Agent 状态）

列表渲染必须**配置与状态分离叠加**：
1. **配置名册读库（Config SSoT）**：名称、Command 等解析 `plugins` / `mcp_servers` 表（插件内嵌 MCP 解析 `mcp_policy`），**禁止仅从内存反推配置**。
2. **连接状态读内存（Status SSoT）**：Connected/Disconnected 与运行时 Error 取自内存 `runtimeMap`，叠加到配置项上。
3. **消除监控死角**：插件因环境故障（命令报错、依赖缺失）未注册进内存时，前端仍展示该记录并高亮断线与报错。

---

## 8.7 自动化中心（`/automation` 页）

> **调度 SSoT 声明**：`cron_jobs`（原 014 表）已彻底废弃删除，调度 SSoT 为 `automations` + `workflows`（ADR-0114）。

### DB 表

| 表 | 用途 | 迁移 |
|----|------|------|
| `automations` / `automation_runs` | 自动化配置（SSoT）/ 执行历史（每次触发一条；含 `source` 列区分来源） | 017 |
| `channels` | Webhook 入站接入（trigger_type=webhook 时关联） | 012 |
| `chat_sessions` | 每次执行独立 session（打 `automation_id` 标签，`source='automation'`） | 013 |
| `workflows` / `workflow_steps` / `workflow_runs` | 工作流定义（含 `result_action`、`source` 列）/ 步骤（含 `step_key`、`kind`、`config_json` 列）/ 执行历史（含步骤级产出摘要） | 029 |
| `workflow_run_data` | 管线步骤级输出数据持久化（State-in-DB，单步 ≤1MiB，保留最近 20 轮，ADR-0114） | 029 |
| `workflow_seen_items` | 管线跨运行条目去重记录（TTL 90天，by url/id，ADR-0114） | 029 |

**automations 关键字段**:

| 字段 | 说明 |
|------|------|
| `trigger_type` | `cron` / `webhook` / `both` / `manual`；另有 `event`（`eventTick` 轮询 `events` 表，按 `event_filter` JSON 匹配 topic/type） |
| `cron_schedule` | 5 字段 cron；trigger_type=webhook 时可空 |
| `channel_id` | 关联 channels.id；webhook 触发时非空 |
| `env_type` | `inline`=当前目录读写（默认）/ `worktree`=Git 隔离分支，可生成 PR |
| `working_dir` | 单一工作目录路径 |
| `reasoning_effort` | `low/medium/high/ultra`，自动映射 model_roles |
| `result_action` | `session`=追加到自动 session / `channel:{id}`=接入推送 / `silent` |
| `source` | `user`=用户创建 / `agent`=Agent 会话通过 `automation_save` 自建 / `plugin`=插件伴生模板实例化 |
| `next_run_at` | 预计算下次触发时间，cronTick 按此列索引 |
| `last_run_status` | `ok/error/running`（防重入） |

**禁止**：`model_id` 不对用户暴露（固定 `auto`），按 `reasoning_effort` 映射。

### 触发模型与 Cron 运行器

| trigger_type | 触发路径 |
|-------------|----------|
| `cron` | `cronTick`（60s 轮询）`next_run_at <= NOW()` 且 `last_run_status != 'running'` → `executeAutomation` |
| `webhook` | `POST /v1/webhooks/{type}/{channelID}` → 平台验签 → `executeAutomation` |
| `both` | cron 与 webhook 独立触发，互不干扰 |
| `manual` | `POST /v1/automations/{id}/trigger` → 202 + `{run_id}`，异步执行 |
| `event` | `eventTick` 复用 cronTick 周期，匹配 `event_filter` |

`startCronRunner(ctx)` 接收可取消 context，由 `Server.Start()` 创建并存入 `Server.cronCancel`，`Shutdown()` 首先调用（§1.2.4）。`cronTick` 对每条到期记录启动独立 goroutine 调 `executeAutomation()`，写 `automation_runs`（含 `prompt_snapshot`），结束时更新 `last_run_status` / `run_count`。实现 `internal/gateway/server/sysadmin/cronadmin/`。

### 与聊天的关系

- 自动化执行**不注入**用户正在进行的聊天流；每次产生独立 `chat_sessions`（会话页标 🤖），可点击"查看会话记录"跳转并继续对话。
- 聊天中说"创建一个每天整理 PR 的自动化"由 M4 识别意图并引导跳转或调内置工具创建（M4 职责）。

### 业务流：HITL 审批

Agent 触发危险操作（`write_network` / Privileged / 超预算）→ M11 Cedar-Gate 拦截，`tasks.status` 置 `Suspended`，SSE 推 `approval_pending`（前端 approvals badge +1）→ 用户在「待办审批」Tab 经 `GET /v1/approvals/pending` 查看后 `POST /v1/approvals/{id}/resolve { decision, note? }` → M8 Blackboard 将 `TaskEntry.status` 更新为 `Running` 或 `Cancelled`。

### 外部触发器（Webhook）

`POST /v1/webhooks/{channelType}/{channelID}` → ChannelManager → 查 `automations WHERE channel_id=channelID AND enabled=1` → 平台级验签（`verifyWebhookSource`，失败 401，fail-closed）→ `executeAutomation(..., "webhook")`。请求体 `MaxBytesReader` 限 4MB，超限 413。验签实现 `internal/gateway/server/sysadmin/channelsadmin/`：

| 平台 | 验签机制 | 必需配置项 |
|------|---------|-----------|
| Slack | HMAC-SHA256（`X-Slack-Signature` v0=，时间戳 ±5min 防重放） | `signing_secret` |
| Discord | Ed25519（`X-Signature-Ed25519` + `X-Signature-Timestamp`） | `public_key` |
| Telegram | 可选直比（`X-Telegram-Bot-Api-Secret-Token`，未配置则放行） | `webhook_secret_token`（可选） |
| WhatsApp | HMAC-SHA256（`X-Hub-Signature-256`） | `app_secret` |
| Teams | clientState 字段比对 | `client_state` |
| Line | HMAC-SHA256（`X-Line-Signature`） | `channel_secret` |
| 其他 | 通用 HMAC-SHA256（`X-Hub-Signature-256`） | `webhook_secret`（未配置→401） |

### 工作流（Workflows）

automations 的**有序链式/DAG 扩展**：将多个任务编排为流水线（`workflows.type = chain | dag`）。API 见 §1.2（`/v1/workflows`）。

- 关键字段：`result_action`（`session` / `channel:{id}` / `silent`，同 automations 语义）；`source`（`user` / `agent` / `plugin`）。
- 触发：`cron`（`cronTickWorkflows` 与 cronTick 同批）/ `manual`（`POST /v1/workflows/{id}/trigger`），均调 `executeWorkflow`。
- `chain`（默认）：忽略 `depends_on`，按 `seq` 顺序执行；上一步 reply 截断至 2000 字符后注入下一步 prompt 前缀（`[前一步骤输出]...`，`input_from_prev`）。
- `dag`：`workflow_steps.depends_on` 非空构造真实 DAG 并行边（AND-Join，元素为 0-based seq 索引字符串）；空则为并行入口；步骤失败重试 `max_retries`（`StateGraphExecutor` 自环边）。图构造 `internal/gateway/server/sysadmin/workflowadmin/workflow_graph.go`，设计 → ADR-0046 决策三、M08 §3-quinquies。
- 每步独立 chat_session，执行历史写 `workflow_runs.step_outputs`（JSON 数组）；电路断路器同 automations（连续 5 次 error → `circuit_open=1`）；步骤可选绑定已有 `automation_id` 或内联 prompt（二选一）。
- 续跑：`POST /v1/workflows/runs/{id}/resume` 续跑 interrupted/error 的运行（202；404/409 语义见 ADR-0104 决策二）。

### 管线步骤类型（Pipeline Steps，ADR-0114）

工作流（`workflows.type = 'dag'`）步骤 `workflow_steps` 支持四种 `kind`（`step_key`、`kind`、`config_json`）：

1. **`tool`（确定性工具执行，0 token）**：
   - 经 `AgentToolExecutor.ExecuteWithTaint` 调用内置工具或 MCP 工具（唯一入口，HE-3）。
   - **允许集**：只读内置工具（`capability=read-only`）；读取类网络白名单（`web_fetch`、`feed_fetch`）；声明只读提示的 MCP 工具（`annotations.readOnlyHint=true`）；写入类白名单（`knowledge_ingest`）。
   - 其余有副作用工具（发帖、bash、write_file、未声明只读的 MCP 工具）默认拒绝，除非显式配置 `"approval": "hitl"`；运行时由 `pipeline.Runner` 在调用前经 `Approver`（workflowadmin 适配 HITL 网关）审批，未声明 hitl 或无审批通道一律拒绝（fail-closed，不依赖保存时校验）；`privileged` 与 `automation_*` 工具永远禁止在步骤中调用。
   - 参数含 `{{steps.*}}` 上游数据引用时以 `TaintHigh` 调用。
2. **`transform`（确定性数据变换，纯 Go，0 token）**：
   - 固定算子集（零表达式语言）：`concat`、`dedup`（可按 run 或跨 workflow 跨轮去重，走 `workflow_seen_items`）、`filter`、`sort`、`limit`、`pick`、`rename`、`truncate`。
   - 依赖均在调用方定义，纯逻辑位于 internal/automation/pipeline/（ADR-0114）。
3. **`llm`（单次提炼推理）**：
   - 单次调用 `Provider.Infer`（default 便宜模型池，默认关闭思考，purpose=`workflow_llm_step`），输入字符上限 24000、输出 token 上限 2000。
   - 前缀稳定（ADR-0105 决策三）：系统提示词为固定模板，数据仅置于 user 消息并加反注入外部数据定界标记（HE-2）。
4. **`agent`（多轮认知循环）**：
   - 既有行为：拉起 Headless Agent 会话执行完整思考循环。仅在必须依赖自主规划时使用；若管线全部由 agent 步骤组成，校验器会给出 Warning 建议改为 tool/transform。

**`skip_if_empty` 机制**：
步骤若配置 `skip_if_empty: true`，当其输出数据条目为 0 时，本步骤及所有下游传递依赖步骤自动标记为 `skipped`；若整个管线所有终点步骤均 skipped，则运行整体状态为 `ok` 且 `skipped=1`，不发起 LLM 调用，不执行任何投递通知（0 token 消耗且不打扰用户）。

**无头来源禁止自建（防失控，HE-7）**：
调用上下文若来源于自动化运行、工作流运行或插件 channel 的无头会话（`protocol.IsBackgroundWork(ctx)` 为真），`automation_save`、`automation_delete`、`automation_run_now` 在工具层直接返回 `CodeForbidden`（`internal/tool/builtin/automation_tools.go`），杜绝自繁殖及外部网页内容通过自动化篡改持久化配置。单 Agent 来源活跃自动化上限由 `thresholds.m13_interface.agent_automation_max` 控制。


---

## 8.8 设置：权限模式与电脑操控（`/settings` → `电脑操控` Tab）

偏好存 `preferences` 表（`key TEXT PK, value TEXT`），经 `PUT /v1/preferences/{key}`（body `{ value }`）写入：

| key | value | 说明 |
|-----|-------|------|
| `computer_use_mode` | `default` \| `auto_review`（默认）\| `full_access` | 设备操控权限模式（前端 `$store.computer`） |
| `computer_any_app_enabled` | `"true"`/`"false"` | 是否允许任意应用 |
| `computer_chrome_enabled` | `"true"`/`"false"` | 是否启用 Chrome 深度交互 |
| `permission_mode` | 同上三值 | 写入 Cedar 上下文 `permission_mode`（`internal/security/policy/gate_builtin_rules.go`） |

- **模式语义**：`full_access`（完全访问）→ `interceptComputerUse()`（`internal/agent/agent_execute_util.go`）不发起 HITL，超时兜底 `auto_approve`；`auto_review` / `default` → 高危操作触发 HITL，超时 `kill_pause`。详见 §2.4 权限模式联动、M07 §7.1。
- **模式切换**：`PUT /v1/preferences/computer_use_mode` UPSERT `preferences` 并热更新 Agent 配置，由 M11 PolicyGate 为当前 Session 重签对应级别 Capability Token。
- **Chrome 深度交互**：启用后 `browser_use` / `computer_use` 经 `interceptComputerUse()` 放行，通过 MCP puppeteer 或 CDP 执行 `click(x,y)` / `type(text)` / `read_dom()`；所有 DOM 读取内容进入 `TaintLevel=High`（外部 Web 内容，inv_M7_02）。

---

## 8.9 前端组件规范（官方推荐写法）

> 约束：UI 组件**优先采用 DaisyUI / Tailwind / Alpine.js 官方推荐的原生结构**；禁止自造等效轮子、禁止在 `style.css` 重复实现已有组件语义。

| 场景 | 官方组件/写法 | 禁止写法 |
|------|-------------|---------|
| **弹窗** | `<dialog class="modal">` + `<div class="modal-box">`，关闭用 `<form method="dialog">` 或 `modal-backdrop` | 自定义 `position:fixed` 遮罩 |
| **标签页** | `<div role="tablist" class="tabs tabs-boxed">` + `<a role="tab" class="tab">`，激活仅加 `tab-active` | `tab-active bg-base-200 shadow-sm` 等手工覆盖导致圆角失效 |
| **抽屉** | 页面级抽屉用 DaisyUI `drawer`：`drawer` → `drawer-toggle` → `drawer-content` → `drawer-side` → `drawer-overlay`；页内临时侧滑面板允许 Tailwind `translate-x-*` 工具类 + Alpine 过渡（ADR-0112 决策八 #5） | 手写 CSS `transform: translateX` 侧滑 |
| **聊天气泡** | `chat chat-start/chat-end` + `chat-bubble` | 自定义 flexbox 对齐 + 气泡背景 |
| **Flex 页面滚动** | `overflow-y-auto` 容器必须同时加 `min-h-0` | 仅 `overflow-y-auto` |
| **表单字段** | `form-control` + `label`/`label-text` + `input input-bordered` | 自定义 `.form-group` 等 class |
| **Badge / Tag** | `badge badge-{variant} badge-{size}` | 手工 `border-radius + padding + color` |
| **空状态** | `flex flex-col items-center justify-center` + SVG + DaisyUI 排版 | 自定义空态组件 |
| **多语言文本** | `x-text="$store.i18n.t('key')"` / `:placeholder=…`，key 在 `i18n.js` 的 `zh`/`en` 字典注册 | 硬编码中文进 HTML 模板 |
| **Emoji + 翻译拼接** | 模板中拼接 `x-text="'📦 ' + $store.i18n.t('key')"`；i18n value 只含纯文字 | i18n value 混入 emoji 导致重复 |
| **全局 CSS** | `style.css` 仅含 Tailwind v4 初始化、DaisyUI 主题声明、少量 `@theme` Token、全局变量覆盖（`--radius-box`） | 重新实现 DaisyUI 已有组件 |

**补充约束**:

| ID | 约束 |
|----|------|
| `inv_webui_12` | 新 Tab 容器必须用 `tabs tabs-boxed`，激活项仅加 `tab-active`，不叠加自定义背景/阴影，保证主题下圆角曲率。 |
| `inv_webui_13` | Flex 布局下的滚动容器（含各子页面顶层 `<div>`）必须同时携带 `overflow-y-auto min-h-0`。 |
| `inv_webui_14` | 新增用户可见字符串必须同时在 `zh` 和 `en` 字典注册，不允许仅注册其一。 |
