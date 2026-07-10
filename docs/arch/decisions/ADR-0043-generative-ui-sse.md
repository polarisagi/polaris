# ADR-0043: Generative UI SSE 集成（结构化组件渲染）

- **状态**: Proposed（未实现，设计草案，由 `docs/specs/09-Generative-UI.md` 迁移并基于现有代码重新设计）
- **日期**: 2026-07-11
- **决策者**: MrLaoLiAI
- **相关模块**: M07 / M13 / `web/`

## 上下文

M13 Web UI 当前仅渲染纯文本 SSE 事件（`token`/`tool_call`/`status` 等字符串字面量事件类型，无形式化 registry，见 `internal/gateway/server/chat/sse.go` 的 `WriteSSE`）与 Tool Result 原始 JSON，缺少 Agent 主动输出结构化 UI（图表/表单）的通道。

前端确认使用 Alpine.js + marked（`web/package.json`），与原草案假设一致；但**当前无 DOMPurify 依赖**——原草案假设"客户端 DOMPurify 净化"是既有能力，实际需要新增依赖，此处予以修正为本决策的前置事项。

## 决策

1. 新增 SSE 事件类型字面量 `"ui_component"`，沿用现有 `WriteSSE(w, flusher, eventType, data)` 无 registry 的字符串字面量模式，不引入新的事件类型枚举机制。
2. 渲染工具（`render_chart`/`render_form`）必须经标准 `protocol.ToolRegistry.ExecuteTool` 入口注册（R1.13 强制，禁止旁路），`SandboxTier=InProcess`（对齐 `internal/tool/builtin/memory_tools.go` 先例——仅格式化 JSON payload，无外部 I/O，无需沙箱隔离）。
3. 渲染工具执行结果双写：先写入 `events` 表（EventLog，HE-Rule-6 State-in-DB），后通过 SSE `ui_component` 广播——顺序不可颠倒，否则崩溃恢复会丢失已推送给前端但未持久化的 UI 状态。
4. 前端白名单校验：`component_type` 必须落在预注册组件白名单（`web/src/js/store/` 新增），未知类型 fallback 为 JSON 展示，不触发渲染；禁止后端下发原生 HTML 供 `x-html` 挂载。
5. **前置依赖缺口**：`render_markdown` 类组件的客户端净化需要 DOMPurify，需在实现 PR 中一并新增依赖（`web/package.json`），不得作为事后补丁；后端侧写入 `events` 表前仍需过 M11 `PIIDetector` + HTML 标签白名单过滤（复用现有安全门）。

## 后果

- **正向**：复用 ToolRegistry/EventLog/PolicyGate 既有基础设施，改动面收敛为"新增两个 InProcess 工具 + 一个 SSE 事件字面量 + 前端组件白名单"，无新增网关/存储机制。
- **负向**：前端新增 DOMPurify 依赖及组件白名单维护成本；SSE 事件类型目前无形式化 registry，`ui_component` 后续若类型增多需评估是否收敛为结构化枚举（超出本 ADR 范围）。
- **反例守护**：未来如有人提议后端直接下发原生 HTML 字符串走 `x-html` 挂载，或渲染工具绕过 `ToolRegistry.ExecuteTool` 直接由 SSE handler 拼装结果，引用本 ADR 拒绝——违反 R1.13（绕过工具注册）与本决策 §4 白名单沙箱前提。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 后端直接下发原生 HTML + 前端 `x-html` 全量挂载 | XSS / UI 劫持面过大，与原草案自身否定的做法一致 |
| 渲染工具走独立 SSE handler 直连，不经 ToolRegistry | 违反 R1.13（绕过 `ExecuteTool` 统一入口），无法获得 PolicyGate/审计能力 |

## 引用代码

- `internal/gateway/server/chat/sse.go`（`WriteSSE`、既有 SSE 事件类型模式）
- `internal/tool/builtin/memory_tools.go`（InProcess SandboxTier 先例）
- `internal/tool/tool.go`（InMemoryToolRegistry — ExecuteTool 五阶段入口）
- `web/package.json`（当前前端依赖：alpinejs + marked，无 DOMPurify）
- `docs/arch/M13-Interface-Scheduler.md §8.3`（Chat SSE 渲染现状）
- `docs/arch/M07-Tool-Action-Layer.md`（ToolRegistry 注册范式）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-07-11 | 初稿，由 `docs/specs/09-Generative-UI.md` 迁移并基于现有代码结构重新设计（补充 DOMPurify 依赖缺口核查、SandboxTier/ToolRegistry 落点） |
