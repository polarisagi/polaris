# ADR-0042: Gateway 交互式提案合集：HITL AskUser 咨询闭环 + Generative UI SSE（含原 ADR-0043）

- **状态**: Proposed（均未实现）| **日期**: 2026-07-11（合并 2026-07-28）| **模块**: M04/M07/M13 `internal/automation/hitl` / `web/`

## 决策一：HITL AskUser 咨询闭环（AskHuman 特权工具，原决策）

不新增平行类型体系，复用现有 `HITLPrompt`/`HITLResponse` + FSM `SuspendReason` 机制支持"咨询"语义：

1. 新增系统级特权工具 `AskHuman(query string) string`，受频率阈值限制防死循环提问。
2. 执行时构造 `HITLPrompt{CheckpointType:"clarification_request"}` 发往既有 `HITLGateway.Prompt()`，不新建平行网关。
3. Agent 挂起复用 `SuspendReason` 新增取值 `awaiting_user_input`，转入既有 S_SUSPENDED 态。
4. `HITLResponse` 新增 `Payload string` 字段承载自由文本回复（破坏性扩展，走 B5.2 全流程）。
5. `Payload` 注入 FSM 上下文前须过 M11 `PIIDetector` + PromptInjectionFilter。

## 决策二：Generative UI SSE 集成（原 ADR-0043，结构化组件渲染）

新增 SSE 事件类型字面量 `"ui_component"`，沿用现有无 registry 的字符串字面量模式。渲染工具（`render_chart`/`render_form`）必须经标准 `ExecuteTool` 入口注册（R1.13，禁旁路），`SandboxTier=InProcess`。执行结果先写 `events` 表（HE-6）后经 SSE 广播，顺序不可颠倒。前端 `component_type` 须落在预注册白名单，未知类型 fallback 为 JSON 展示；禁止后端下发原生 HTML 供 `x-html` 挂载。需新增 DOMPurify 依赖用于客户端净化。

## 反例守护

拒绝新建独立 `ClarificationRequest` 类型或独立 Suspend 错误类型/独立挂起态——HITL 网关是单一入口（M13 §2.4），拆分违反可组合原语。拒绝后端直接下发原生 HTML + 前端全量 `x-html` 挂载——XSS 面过大。拒绝渲染工具绕过 `ToolRegistry.ExecuteTool` 直连 SSE handler——违反 R1.13。

## 引用代码

`internal/automation/hitl/gateway.go`、`pkg/types/models_other.go`（HITLPrompt/HITLResponse）、`internal/gateway/server/chat/sse.go`、`internal/tool/tool.go`
