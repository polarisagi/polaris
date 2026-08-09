# ADR-0085: 抽取 SessionOrchestrator 领域层收敛会话生命周期编排（GD-13-008）

- **状态**: Accepted（已执行）| **日期**: 2026-08-01 | **模块**: M13 `internal/gateway/session/`

## 上下文

`internal/gateway/server/chat/sse.go` 的 `HandleAgentStream` 曾把整段对话生命周期编排（会话确保/历史加载/Hook 分发/斜线命令路由/压缩决策/SystemPromptGuard 装配/消息持久化/Transcript 写入/FSM 驱动）放在 HTTP 适配层内。SSE（交互式）与 Headless（Cron/Workflow/Webhook）两条入口各自重复实现同一套编排逻辑，历史代价：`SystemPromptGuard` 曾只接在 SSE 路径，Headless 路径遗漏（`internal/agent/pool.go` 的 `headlessPromptGuard` 补丁注释是"各自实现导致防护漏接"的直接物证）。原规划设想的第三条"非流式"入口（`server_handlers.go:136`）经核实从未以会话编排重复实现的形态存在——自始至终是异步 Blackboard 任务投递，与本次收敛无关（见 `local_playground/upgrade/99-new-findings.md`），故本决策实际收敛范围调整为 SSE + Headless 两条入口，由 `Request.Headless` 字段区分子路径。

## 决策

新建 `internal/gateway/session` 领域包承载 `Orchestrator` 接口（`RunTurn(ctx, Request, Sink) (*Result, error)`），HTTP/SSE 层与 Headless 调用方共享同一套编排实现。

**包位置排除项**：
- 不放 `internal/agent`（L1）——会话编排需消费 `Hooks`/`SlashRouter`/`CompressionEngine`/`Persistence`/`protocol.LLMRegistry` 等 L3 接口层能力，放入 L1 会造成 L1→L3 反向依赖。
- 不放 `internal/gateway/server/chat`——`chat` 是 HTTP handler 包，收敛后需被 `agent/pool.go`（L1）间接消费，必须是零 HTTP 依赖的纯领域包。
- 不放 `internal/execute`——`internal/execute/CLAUDE.md` + ADR-0046 明确 execute 只负责"如何跑完一份已确定的计划/图，不做决策"；会话编排包含决策（是否压缩、是否短路斜线命令），不属于该层。

**Sink 抽象**：`Orchestrator` 只产出领域语义事件（`Event{Kind, Text, Payload, Err}`，`Kind` 枚举 `KindDelta`/`KindReasoning`/`KindStatus`/`KindContextWarning`/`KindToolCall`/`KindComplete`/`KindError`/`KindSystemNotice`），不感知 SSE 帧格式或 HTTP 状态码。HTTP/SSE 层（`chat/sse_sink.go`）反向实现 `session.Sink` 把领域事件翻译为既有 SSE wire 事件名；Headless 调用方用 `BufferSink` 纯内存累积增量文本，忽略状态类事件展示。

**消费端接口**（HE-3，接口在调用方 `session` 包定义）：`HookRunner`/`SlashDispatcher`/`CompressionEngine`/`Persistence`/`PromptAssembler` 均为窄接口，`chat` 包（SSE 入口）与 `cmd/polaris`（Headless 装配层）分别提供满足这些接口的适配器，`session` 包不直接 import `chat` 包具体类型，避免 `chat→session→chat` 循环依赖。

**两条防退化 lint**（`internal/lint/inv_lint_test.go`）：
1. `Test_inv_M13_SessionPkgNoHTTP`：`internal/gateway/session/` 下禁止出现任何 `net/http` 依赖。
2. `Test_inv_M13_SingleTurnEntry`：`SetTaskIntent(...)` + `SendIntent(types.TriggerIntentReceived)`（驱动一轮 Agent FSM 的底层触发原语）只允许出现在三处固定位置——`internal/gateway/session/`（唯一会话编排入口）、`internal/agent/pool.go`（Agent 生命周期原语，非会话编排）、`cmd/polaris/boot_crash_recovery.go`（崩溃回放）；其余调用点须登记进 `internal/lint/testdata/turn_entry_exempt.json` 并附豁免理由，防止"第三方会话编排重复实现"静默重新出现。

## 后果

- **正向**：SystemPromptGuard 类防护漏接问题物理消除（单一实现，两条入口共享）；新增会话侧能力（压缩策略/Hook 事件）只需改一处。
- **负向**：`session` 包依赖面较宽（5 个消费端接口），新增编排步骤需评估是否两条入口都需要感知。
- **反例守护**：拒绝在 `chat` 包或 `agent/pool.go` 内新增第二套"确保会话/加载历史/驱动 FSM"的重复实现——违反本决策的收敛动机，且会被 `Test_inv_M13_SingleTurnEntry` 拦截。拒绝让 `session` 包引入 `net/http`——违反零依赖约束，会被 `Test_inv_M13_SessionPkgNoHTTP` 拦截。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 编排逻辑保留在 `chat/sse.go`，Headless 侧调用 chat 包导出函数 | `chat` 包仍是 HTTP handler 包，Headless（`agent/pool.go`，L1）反向 import 会造成 L1→L3 依赖，且无法摆脱 HTTP 类型 |
| 把编排逻辑下沉到 `internal/agent` | 需要消费 L3 层的 Hooks/SlashRouter/CompressionEngine/Registry，会形成 L1→L3 反向依赖 |
| 归入 `internal/execute` | execute 层职责是"跑完已确定的计划"，不做决策；会话编排包含"是否压缩/是否短路斜线命令"等决策逻辑，职责不匹配（ADR-0046） |

## 引用代码

- `internal/gateway/session/orchestrator.go`（`Orchestrator`/`Deps`/`RunTurn`）
- `internal/gateway/session/types.go`（`Event`/`Sink`/`Request`/`Result`/五个消费端接口）
- `internal/gateway/session/sink.go`（`BufferSink`，Headless 用）
- `internal/gateway/server/chat/sse_sink.go`（SSE 侧 `session.Sink` 实现）
- `internal/lint/inv_lint_test.go`（`Test_inv_M13_SessionPkgNoHTTP`、`Test_inv_M13_SingleTurnEntry`）
- `local_playground/upgrade/04-arch-refactor.md` §A-03（原始设计记录）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-08-01 | 初稿，阶段06 ADR 归档补录（对应阶段04 A-03 已落地实现） |
| 2026-08-09 | 追记：重新评估触发条件——任何在 `chat` 包或 `agent/pool.go` 内新增"确保会话/加载历史/驱动 FSM"重复实现的提议，须先说明为何不能复用 `session.Orchestrator`；`Test_inv_M13_SingleTurnEntry`/`Test_inv_M13_SessionPkgNoHTTP` 两条 lint 是本决策的强制执行机制，放宽须同步修改这两条测试并说明理由。 |
