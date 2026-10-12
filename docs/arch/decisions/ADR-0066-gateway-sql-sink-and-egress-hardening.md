# ADR-0066: Gateway 治理

> 状态：Accepted（决策一~六已执行）/ Proposed（决策七未实现） ｜ 首次：2026-07-08 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0039/0064/0067/0071/0085/0042/0043 ｜ 模块：M04 / M13 `internal/gateway/` / `internal/channel/`

## 决策一：Gateway 控制权移交 FSM（原 ADR-0039、原 ADR-0066 决策一）

- 废除 Agent 端点绕过中央 FSM 自行编排 LLM 与工具循环的"MVP 直通模式"（违反 HE-5，SurpriseIndex/ThinkingMode/EventLog 空转）。
- FSM 原生发 `AgentStreamEvent`，Gateway 经 `AgentController` 订阅并中继 SSE，只做协议转换。
- `/v1/...` OpenAI 兼容代理路径（无 Agent 语义）豁免。

## 决策二：直连 SQL 下沉 Repository（原 ADR-0066 决策二，A-4）

- Gateway 控制层六处直执 SQL（cron_runner / cron_handlers / cron_scheduler / cron_templates_handlers / chat sessions / channelsadmin webhook_receive）改调 `AutomationRepository` 等 DAO（R1.1 ctrl→svc→dao）。
- 范围仅限这六处；`internal/channel/manager.go`、`sysadmin/*` 等处裸 SQL 不背书也不禁止，扩大须单独评估，不得默认套用。

## 决策三：EgressGateway 默认白名单收紧（原 ADR-0066 决策三，A-6）

- `DefaultAllowedDomains()` 不含 `localhost`/`127.0.0.1`（M13 不默认放行环回，纵深防御；M11 SafeDialer 另有兜底）。本地调试运行时动态注入。代码核实：现白名单仅 Provider/GitHub 类公网域名。

## 决策四：Channel 适配器注册表（原 ADR-0064、原 ADR-0066 决策四，A-2）

- `adapter.Adapter` 接口（`Type`/`Extract`/`Send`/`StartPoller`）取代 dispatch/message/manager 三处 ~17 平台的 `switch channelType`；`GetAdapter` 收敛为 `adapter.go` 内一处 switch，各平台为 `sync.OnceValue` 惰性单例（`inv_NoGlobalVar` 既定豁免类别）。
- 否决"包级全局 map + init() 自注册"（违反禁全局可变变量）与"每次新建实例的 factory-switch"（`wecomSends`/`txnCounter` 等跨调用状态被清空，wecom 回复丢失、matrix 事务 ID 重复）。
- Poller 入站消息经 `Manager.SetMessageHandler`（`atomic.Pointer`）在 `boot_server.go` 初始化后立即接线（此前回调链为空函数，入站丢失）。

## 决策五：SessionOrchestrator 收敛会话生命周期编排（原 ADR-0085，GD-13-008；落地原 ADR-0067 的 God Class 拆分路线）

现行状态：原 ADR-0067 拟议的 `ChatOrchestrator` 以 `internal/gateway/session` 包落地，`chat/sse.go` 已回归 HTTP 边界层（约 150 行）；ADR-0067 路线第③步完成，不再有"待排期"。

- `session.Orchestrator.RunTurn(ctx, Request, Sink)` 承载会话确保 / 历史加载 / Hook 分发 / 斜线命令路由 / 压缩决策 / SystemPromptGuard 装配 / 持久化 / Transcript / FSM 驱动；SSE 与 Headless（Cron/Workflow/Webhook）共用，由 `Request.Headless` 区分子路径。
- 起因：两条入口各自实现导致 `SystemPromptGuard` 只接 SSE、Headless 漏接。原设想的第三条"非流式"入口核实从未以重复编排形态存在（异步 Blackboard 投递），不在范围。
- 包位置：独立零 HTTP 依赖领域包。不放 `internal/agent`（L1→L3 反向依赖）、不放 `chat`（HTTP handler 包，L1 不可反向 import）、不放 `internal/execute`（只"跑已确定的计划"，不做决策，ADR-0046）。
- Sink 抽象：Orchestrator 只产领域事件 `Event{Kind,Text,Payload,Err}`（Kind：Delta/Reasoning/Status/ContextWarning/ToolCall/Complete/Error/SystemNotice），不感知 SSE 帧或 HTTP 状态码；SSE 侧 `chat/sse_sink.go` 实现 `session.Sink`，Headless 用 `BufferSink`。
- 消费端接口（HE-3，调用方 `session` 包定义）：`HookRunner`/`SlashDispatcher`/`CompressionEngine`/`Persistence`/`PromptAssembler`；`chat` 与 `cmd/polaris` 提供适配器，避免 `chat→session→chat` 循环。
- 门控（强制执行机制，放宽须同步改测试并说明理由）：
  - `Test_inv_M13_SessionPkgNoHTTP`：`session/` 禁 `net/http`。
  - `Test_inv_M13_SingleTurnEntry`：`SetTaskIntent` + `SendIntent(TriggerIntentReceived)` 仅允许在 `internal/gateway/session/`、`internal/agent/pool.go`、`cmd/polaris/boot_crash_recovery.go`；其余登记 `turn_entry_exempt.json` 并附理由。
- 负向后果：`session` 依赖面较宽（5 个消费端接口），新增编排步骤须评估两条入口是否都需感知。

## 决策六：Downloader 公网出站豁免（原 ADR-0071，XR-06）

- `internal/downloader/proxy.go` 的 `canReachGitHub` 等豁免 M11 SafeDialer，允许裸 `http.DefaultTransport`：SafeDialer 不走系统 TUN、不读 `HTTPS_PROXY`，受限网络区域须依赖系统代理触达 GitHub；目标 URL 静态写死、非用户可控，不构成 SSRF。
- CI 强制：`Test_inv_XR06_DownloaderNoRawTransport` 扫 `internal/downloader/` 裸 Transport，`xr06_raw_transport_exempt.json` 登记 `proxy.go` 为唯一豁免；未登记文件新增即失败。
- 仅适用静态写死公共域名；任何用户输入/动态拼接 URL 仍受 SafeDialer 约束，一旦域名变为动态须立即撤回豁免，无例外。新增静态域名先入 CI 白名单并说明理由，不放宽判定标准。

## 决策七：HITL AskUser 咨询闭环 + Generative UI SSE（原 ADR-0042 决策一/二，原 ADR-0043，Proposed）

核实（2026-10-12，rg `AskHuman|clarification_request|awaiting_user_input|ui_component|render_chart|render_form` 于 internal/pkg/web/cmd 零命中）：**两项均未实现**，状态仍为 Proposed。相邻已实现机制不等同于本提案：

- 回合内人工审批已落地（ADR-0098 决策十）：`status{type:"approval_required"}`（`session/orchestrator_fsm.go`）→ Web 就地审批卡片 → `POST /v1/approvals/{id}/resolve`；解决的是 Taint 复核，不是 Agent 主动自由文本提问。
- MCP elicitation 与 `tool_ui`（MCP Apps）归 ADR-0103，是 MCP 服务端发起的表单/组件通道，非 Agent 自发的 AskHuman，也非 `ui_component`。

提案要点（保持不变）：

- AskHuman：系统级特权工具 `AskHuman(query)`，频率阈值防死循环；构造 `HITLPrompt{CheckpointType:"clarification_request"}` 发既有 `HITLGateway.Prompt()`（不新建平行网关）；挂起复用 `SuspendReason` 新值 `awaiting_user_input`（入既有 S_SUSPENDED）；`HITLResponse` 新增 `Payload string`（破坏性扩展，走 B5.2）；Payload 注入 FSM 前过 `PIIDetector` + PromptInjectionFilter。
- Generative UI：SSE 事件字面量 `"ui_component"`；渲染工具（`render_chart`/`render_form`）必须经 `ExecuteTool`（R1.13），`SandboxTier=InProcess`；结果先写 `events` 表（HE-6）再 SSE 广播；前端 `component_type` 落预注册白名单，未知回落 JSON 展示；禁后端下发原生 HTML 供 `x-html` 挂载；需新增 DOMPurify（`web/package.json` 现无该依赖，实施前复核）。

## 反例守护 / 已驳方案

- 保留双模式兼容旧客户端（直通+FSM） — 维护开销加倍且认知层对直通请求不可见 — 2026-07-08
- 仅把 Gateway 循环下沉为 FSM 工具库而不改事件驱动 — 不解决层级违规根因 — 2026-07-08
- 编排留在 `chat/sse.go`、Headless 调 chat 导出函数 — L1→L3 依赖且无法摆脱 HTTP 类型 — 2026-08-01
- 编排下沉 `internal/agent` — 需消费 L3 Hooks/SlashRouter/CompressionEngine，L1→L3 反向依赖 — 2026-08-01
- 编排归入 `internal/execute` — 执行层不做决策，职责不匹配（ADR-0046） — 2026-08-01
- 在 `chat` 或 `agent/pool.go` 新增第二套"确保会话/加载历史/驱动 FSM" — 违反收敛动机，被 SingleTurnEntry 拦截 — 2026-08-01
- 新建独立 `ClarificationRequest` 类型/Suspend 错误类型/挂起态 — HITL 网关单一入口（M13 §2.4） — 2026-07-11
- 后端下发原生 HTML + 前端全量 `x-html` — XSS 面过大 — 2026-07-11
- 渲染工具绕过 `ToolRegistry.ExecuteTool` 直连 SSE handler — 违反 R1.13 — 2026-07-11

## 重新评估触发条件

- 任何在 `chat` 或 `agent/pool.go` 新增会话编排的提议，须先说明为何不能复用 `session.Orchestrator`。
- 决策二六处之外裸 SQL 扩大范围前单独评估。
- 决策六豁免范围扩大走 CI 白名单；动态 URL 立即撤回。
- 决策七实施前核实 `web/package.json` 的 DOMPurify 现状；仅当复用 `HITLPrompt`/`HITLResponse` 不足以支撑自由文本咨询时才重议平行类型体系。

## 沿革

| 日期 | 变更 | 原编号/理由 |
|---|---|---|
| 2026-07-08 | 决策一/二/三 | 原 ADR-0039 及本 ADR 原决策 |
| 2026-07-11 | 决策七提出（Proposed） | 原 ADR-0042/0043 |
| 2026-07-23 | 决策四、决策六 | 原 ADR-0064、ADR-0071 |
| 2026-08-01 | 决策五 SessionOrchestrator 落地 | 原 ADR-0085 |
| 2026-10-12 | 并入 0085/0071/0042；原决策五"ChatOrchestrator 拆分路线（原 ADR-0067，Proposed 暂不实施）"取代为现行决策五（已由 session 包落地，`sse.go` 已瘦身） | 文档合并 |

## 引用代码

`internal/agent/agent_execute_effect.go`、`internal/gateway/session/orchestrator.go`、`internal/gateway/session/types.go`、`internal/gateway/session/sink.go`、`internal/gateway/server/chat/sse.go`、`internal/gateway/server/chat/sse_sink.go`、`internal/protocol/repo/repo_automation.go`、`internal/gateway/egress/egress_gateway.go`、`internal/channel/adapter/adapter.go`、`internal/channel/manager.go`、`internal/downloader/proxy.go`、`internal/lint/inv_lint_test.go`、`internal/lint/testdata/turn_entry_exempt.json`、`internal/lint/testdata/xr06_raw_transport_exempt.json`、`internal/automation/hitl/gateway.go`、`pkg/types/models_other.go`、`cmd/polaris/boot_server.go`
