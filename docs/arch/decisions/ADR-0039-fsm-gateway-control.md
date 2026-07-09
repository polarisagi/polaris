# ADR-0039: Gateway 控制权移交 FSM（废除 MVP 直通模式）

- **状态**: Accepted
- **日期**: 2026-07-08
- **决策者**: 架构组
- **相关模块**: M04（Agent Kernel / FSM）/ M13（Gateway / SSE）/ `internal/agent/` / `internal/gateway/`
- **关联**: [ADR-0022](./ADR-0022-thinking-mode-three-tier.md)（ThinkingMode 路由）| [HE-5]（状态机持控制流）

## 上下文

历史实现中，Gateway 通过 `HandleAgentStream` 和 `runToolInferenceLoop`（即"MVP 直通模式"）直接管理 Agent 交互循环——Gateway 绕过中央 FSM 架构，独立编排 LLM 调用与工具执行循环。

该实现导致三项架构违规：

1. **违反 HE-5**：FSM 必须主导状态转移，而 Gateway 直接驱动对话流程，绕过了 FSM 的状态管控。
2. **L3 层执行 L1 层职责**：Gateway（L3 接口/治理层）承担了 Agent 认知循环（L1 认知/执行层）的工作，层级职责混淆。
3. **核心机制空转**：SurpriseIndex 评估、ThinkingMode 路由（ADR-0022）、EventLog 记录等核心机制在 MVP 直通路径下完全被绕过，实际上沦为"死代码"，对终端用户不生效。

## 决策

**彻底废除 Gateway Agent 端点（`HandleAgentStream`）的 MVP 直通模式，将控制权归还 Agent FSM。**

**1. FSM 原生流式能力**

FSM 现在负责管理对话并流式推送事件。在推理过程中，FSM 原生发出 `AgentStreamEvent`，无需 Gateway 干预。

**2. Gateway 订阅模式**

Gateway 通过 `AgentController` 接口订阅 FSM 的事件流，将接收到的结构化事件中继转换为 SSE 输出。Gateway 仅负责协议转换，不参与认知逻辑。

**3. OpenAI 兼容代理路径豁免**

`/v1/...` 路径（纯代理路由，无 Agent 语义）**不受本次变更影响**。其职责是严格的协议转换，属于 Gateway 层（非认知层），保留直通行为符合架构定位。

## 后果

- **正向**: 修正层级违规，正确重新激活 SurpriseIndex、ThinkingMode、EventLog 等认知组件；将所有对话执行统一到单一可测试的 FSM 状态模型下
- **负向**: 由于流式 Token 从 FSM 到 Gateway 采用事件总线式传播，引入微小的架构延迟
- **反例守护**:
  - 未来如有人提议"保留双模式（MVP 直通 + FSM 并存）以兼容旧客户端"——本 ADR 拒绝。双路径增加维护开销，且认知层对直通请求完全不可见
  - 未来如有人提议"将 Gateway 的 `runToolInferenceLoop` 下沉为 FSM 工具库"——本 ADR 拒绝。仅移动过程式循环代码而不改为真正的事件驱动状态转移，不解决根本违规问题

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保留双模式（MVP 直通 + FSM 并存） | 维护开销加倍；认知层对直通运行不可见，碎片化行为不可测 |
| 将 Gateway 循环下沉为 FSM 工具库 | 仅移动过程式代码，未采用真正的事件驱动状态转移；不符合 FSM 严格事件驱动的转移语义 |

## 引用代码

- `internal/agent/agent_execute.go`（FSM 原生流式推理 + `AgentStreamEvent` 发出）
- `internal/protocol/interfaces.go`（`AgentController` 接口定义）
- `internal/gateway/server/chat/sse.go`（Gateway 订阅 FSM 事件流 + SSE 中继）
- `internal/gateway/server/server.go`（`HandleAgentStream` 入口，MVP 直通模式废除点）
- `docs/arch/M04-Agent-Kernel.md`（FSM 状态机控制流设计）
- `docs/arch/M13-Interface-Scheduler.md`（Gateway 层职责边界）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-08 | 初稿，Accepted |
| 2026-07-09 | 全文翻译为中文；补完整标准 header 字段（决策者/相关模块/关联/被驳回方案表格/引用代码/修订记录）|
