# ADR-0039: Gateway 控制权移交 FSM（废除 MVP 直通模式）

- **状态**: Accepted | **日期**: 2026-07-08 | **模块**: M04 FSM / M13 Gateway

## 决策

废除 Gateway Agent 端点（`HandleAgentStream`）绕过中央 FSM 独立编排 LLM 调用与工具循环的"MVP 直通模式"（违反 HE-5，且使 SurpriseIndex/ThinkingMode/EventLog 等核心机制空转）。FSM 原生发出 `AgentStreamEvent`，Gateway 经 `AgentController` 接口订阅事件流并中继为 SSE，仅做协议转换。`/v1/...` OpenAI 兼容代理路径（无 Agent 语义）豁免，不受影响。

## 反例守护

拒绝保留双模式兼容旧客户端——维护开销加倍且认知层对直通请求不可见。拒绝仅将 Gateway 循环下沉为 FSM 工具库而不改为真正事件驱动——不解决层级违规根因。

## 引用代码

`internal/agent/agent_execute.go`、`internal/gateway/server/chat/sse.go`
