# Specification: HITL AskUser 闭环交互

## 1. 动机与目标
当前 HITL (Human-In-The-Loop) 机制主要用于安全审批（拦截风险操作）或长时任务确认。然而，当 Agent 面临需求模糊、信息缺失或二义性时，缺乏一种主动向用户发起提问并获取纯文本回复的"咨询"能力。
本设计引入 `AskHuman` 交互闭环，扩展 HITL 的表达能力，使其不仅能处理 `true/false` 审批，还能承载 `ClarificationRequest` 的双向通信。

## 2. 设计方案

### 2.1 特权工具：AskHuman
内置一个系统级特权工具 `AskHuman(query string) string`。
- **权限边界**：不受常规预算限制，但受频率拦截（连续调用不超过 M4Kernel 的阈值，防止死循环提问）。
- **执行语义**：当 Agent 执行 `AskHuman` 时，Agent 进入 `Suspend` 状态，触发层向外层抛出带有关起因的事件。

### 2.2 ClarificationRequest 类型定义
在 `pkg/types` (或 `internal/protocol`) 中新增 `ClarificationRequest` 类型，区别于原有的 `ApprovalRequest`。
```go
type ClarificationRequest struct {
    TaskID      string
    Query       string // Agent 的提问文本
    Context     string // 上下文提示（可选）
    Urgency     types.UrgencyLevel
}
```

### 2.3 HITLResponse 扩展
目前的 `HITLResponse` 仅包含布尔值（Approve/Reject）。扩展 `HITLResponse` 引入 `Payload string` 用于承载用户文本输入。
```go
type HITLResponse struct {
    Approved bool
    Payload  string // 用户输入的文本（拒绝时也可用于说明原因）
    Reason   string // 补充系统拒因
}
```

### 2.4 Suspend / Resume 接线方式
- **Suspend**：`AskHuman` 执行器内部不再进行轮询或长轮询，而是向 `HITLGateway` 发送 `ClarificationRequest` 后，立即返回一个特殊的中断错误 `ErrSuspendForInput`，令 FSM 退出至 `S_SUSPENDED`。
- **Resume**：外部 API 接收到用户输入后，调用 `AgentPool.Resume(sessionID, HITLResponse{Approved: true, Payload: "..."})`。FSM 被重新唤醒，状态转入 `S_REPLAN`，并将用户的 `Payload` 注入历史 Context 中。

## 3. 安全与限制
1. 连续 `AskHuman` 保护：引入 `MaxConsecutiveAsk` 阈值，若连续提问未采纳行动，则强制回落人工接管。
2. 数据净化：用户返回的 `Payload` 在注入 FSM 上下文前，需要经过 M11 边界的 `PIIDetector` 和 `PromptInjectionFilter` 处理，防范自激或恶意诱导。
