# ADR-0042: HITL AskUser 咨询闭环（AskHuman 特权工具）

- **状态**: Proposed（未实现，设计草案，由 `docs/specs/08-HITL-AskUser.md` 迁移并基于现有代码重新设计）
- **日期**: 2026-07-11
- **决策者**: MrLaoLiAI
- **相关模块**: M04 / M13 / `internal/agent/fsm` / `internal/automation/hitl`

## 上下文

现有 HITL（`internal/automation/hitl.GatewayImpl` + `types.HITLPrompt`/`types.HITLResponse`，见 `docs/arch/M13-Interface-Scheduler.md §2.4`）只覆盖"审批"语义（Approve/Reject 布尔决策）。Agent 面临需求模糊或信息缺失时，没有"主动提问 + 接收自由文本回复"的能力，只能强行 Plan 猜测或直接失败。

原 `docs/specs/08-HITL-AskUser.md` 草案提议新建平行的 `ClarificationRequest` 类型、独立 `ErrSuspendForInput` 错误与独立挂起态。经核对现有代码（`pkg/types/models_other.go:102`/`:141`、`internal/agent/fsm/state_machine.go:133`）发现：`types.HITLPrompt`/`HITLResponse` 已是唯一审批入口，FSM 已有 `SuspendReason` 字符串枚举机制（`capability_gap`/`provider_exhausted` 先例），原草案的类型假设与实际结构不符，此处予以修正。

## 决策

不新增平行类型体系，复用现有 HITLPrompt/HITLResponse + FSM SuspendReason 机制，仅新增一种"咨询"语义取值：

1. 新增系统级特权工具 `AskHuman(query string) string`，受 M4Kernel 频率阈值限制（连续调用上限，防死循环提问），不占常规工具预算。
2. `AskHuman` 执行时构造现有 `types.HITLPrompt{CheckpointType: "clarification_request", PromptText: query}` 发往 `HITLGateway.Prompt()`——复用同一审批队列/Notifier/超时策略，不新建平行网关路径。
3. Agent 侧挂起复用现有 `SuspendReason` 字段，新增取值 `awaiting_user_input`，FSM 转入既有 S_SUSPENDED 态；不新增独立错误类型或独立挂起态。
4. 用户自由文本回复复用现有 `types.HITLResponse`：当前字段为 `OptionKey/UserID/Approved/Reason`，无自由文本承载字段，需破坏性扩展——新增 `Payload string` 字段，走 `04-Module-Boundary.md B5.2` 流程（独立 commit + 同 PR 同步全部 producer/consumer）。Resume 路径复用/新建 `AgentPool.Resume`，将 `Payload` 注入历史 Context 后转入 S_REPLAN。
5. `Payload` 注入 FSM 上下文前必须过 M11 `PIIDetector` + PromptInjectionFilter（复用现有安全门，不新增平行校验路径）。

## 后果

- **正向**：零新增网关/存储机制，全部复用 M13 HITL 既有基础设施（队列、Notifier、超时策略、审计），改动面收敛到"新增 CheckpointType 取值 + HITLResponse.Payload 字段 + AskHuman 工具"。
- **负向**：`HITLResponse` 字段扩展需走 B5.2 全流程（ADR + producer/consumer 同步 + CHANGELOG），非纯加法；前端 `/v1/approvals/pending` 展示需按 CheckpointType 区分渲染（Approve/Reject 按钮 vs 文本输入框）。
- **反例守护**：未来如有人提议为"咨询"场景新建独立 `ClarificationRequest`/`HITLResponse` 平行类型体系，或新建独立 Suspend 错误类型/独立挂起态，引用本 ADR 拒绝——HITL 网关是单一入口（M13 §2.4），拆分平行路径违反 R3-HE3 可组合原语。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 新建独立 `ClarificationRequest` 类型 + 独立网关路径（原 `docs/specs/08-HITL-AskUser.md` 方案） | 与现有 `HITLPrompt`/`HITLResponse` 完全重叠语义，制造两套并行审批基础设施，违反 M13 §2.4 单一网关原则 |
| 新建 `ErrSuspendForInput` 独立错误类型 + 独立挂起态 | FSM 已有 `SuspendReason` 枚举机制（`capability_gap`/`provider_exhausted` 先例），新增取值即可，无需新错误类型/新状态 |

## 引用代码

- `internal/automation/hitl/gateway.go`（GatewayImpl，现有 HITL 网关实现）
- `pkg/types/models_other.go:102`（HITLPrompt）/ `:141`（HITLResponse，本 ADR 待扩展）
- `internal/agent/fsm/state_machine.go:133`（SuspendReason 字段）
- `docs/arch/M04-Agent-Kernel.md §2`（Suspend-on-Idle Actor）
- `docs/arch/M13-Interface-Scheduler.md §2.4`（HITL [ESCALATE]）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-11 | 初稿，由 `docs/specs/08-HITL-AskUser.md` 迁移并基于现有代码结构重新设计（原文档假设的类型体系与实际代码不符，已修正） |
