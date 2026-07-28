# ADR-0066: Gateway 治理合集（控制权移交 FSM + SQL 下沉 + Egress 收紧 + Channel 适配器重构 + ChatOrchestrator 拆分路线，含原 ADR-0039/0064/0067）

- **状态**: Accepted（决策一/二/三/四已执行）/ Proposed（决策五暂不实施）| **日期**: 2026-07-08（其余 2026-07-23，合并 2026-07-28）| **模块**: M04 FSM / `internal/gateway/server/` / `internal/gateway/egress/` / `internal/channel/`

## 决策一：Gateway 控制权移交 FSM（原 ADR-0039，废除 MVP 直通模式）

废除 Gateway Agent 端点（`HandleAgentStream`）绕过中央 FSM 独立编排 LLM 调用与工具循环的"MVP 直通模式"（违反 HE-5，且使 SurpriseIndex/ThinkingMode/EventLog 等核心机制空转）。FSM 原生发出 `AgentStreamEvent`，Gateway 经 `AgentController` 接口订阅事件流并中继为 SSE，仅做协议转换。`/v1/...` OpenAI 兼容代理路径（无 Agent 语义）豁免，不受影响。

## 决策二：直连 SQL 下沉 Repository（A-4，原决策）

Gateway 控制层六个明确站点（`cron_runner.go`/`cron_handlers.go`/`cron_scheduler.go`/`cron_templates_handlers.go`/`chat/sessions.go`/`channelsadmin/webhook_receive.go`）的直执 SQL 改为调用 `AutomationRepository` 等既有 DAO（R1.1 ctrl→svc→dao 分层）。范围明确限定这六处，`internal/channel/manager.go` 与 `sysadmin/{plugin,provider,workflowadmin,...}` 等模块的裸 SQL 不在本次范围内，不背书也不禁止。复核顺带修复 `TimeoutRuns` 引用不存在的 `error` 列（应为 `error_msg`）导致的静默失败死 Bug。

## 决策三：EgressGateway 收紧默认白名单（A-6，原决策）

`EgressGateway.DefaultAllowedDomains()` 移除硬编码的 `"localhost"`/`"127.0.0.1"`——M13 层不应默认放行环回地址（纵深防御，即便 M11 SafeDialer 有兜底）。本地调试改为运行时动态注入。

## 决策四：Channel 适配器注册表重构（原 ADR-0064，A-2）

`dispatch.go`/`message.go`/`manager.go` 三处针对 ~17 个平台的巨型 `switch channelType` 违反开闭原则。引入 `adapter.Adapter` 接口（`Type`/`Extract`/`Send`/`StartPoller`），`GetAdapter` 收敛为 `adapter.go` 内一处 `switch`，各平台返回 `sync.OnceValue` 惰性单例（`Test_inv_NoGlobalVar` 既定豁免类别）。逐平台迁移，原 `switch` 作 fallback，全部完成后移除。

**订正**：初版曾尝试"包级全局 map + 各平台 init() 自注册"，与禁全局可变变量红线冲突；退化为无状态 factory-switch（每次新建实例）又导致 `wecomSends`/`txnCounter` 等跨调用持久状态被静默清空（wecom 回复丢失、matrix 消息可能因事务 ID 重复被去重）——`sync.OnceValue` 单例同时解决合规与状态持久两个问题。

同时修复 Poller 模式入站消息丢失：`adapter -> host.OnMessage -> Manager.onMessage` 回调链此前接成空函数，新增 `Manager.SetMessageHandler`（`atomic.Pointer`），`boot_server.go` 初始化后立即接线。

## 决策五：Gateway God Class 拆分路线（原 ADR-0067，Proposed，暂不实施）

`sse.go` 已演变为承载协议处理+输入预处理（`@file` 展开）+上下文压缩调度+消息持久化+Slash 命令解析的 God Class（GD-13-008 职责过载）。拟引入独立 `ChatOrchestrator` 组件接管全部业务逻辑，Gateway 回归薄网关（仅协议/鉴权/限流/路由/SSE 编解码）。

分三步落地：① SQL 下沉（决策二，已完成）② 统一入站分发（决策四，已完成）③ 抽取 ChatOrchestrator 核心逻辑（**待排期，本轮不实施**，因 `sse.go` 是高频热路径，big-bang 重构风险过大）。

## 反例守护

拒绝保留双模式兼容旧客户端——维护开销加倍且认知层对直通请求不可见。拒绝仅将 Gateway 循环下沉为 FSM 工具库而不改为真正事件驱动——不解决层级违规根因。

## 引用代码

`internal/agent/agent_execute.go`、`internal/gateway/server/chat/sse.go`、`internal/protocol/repo/repo_automation.go`、`internal/gateway/egress/egress_gateway.go`、`internal/channel/adapter/adapter.go`、`internal/channel/manager.go`
