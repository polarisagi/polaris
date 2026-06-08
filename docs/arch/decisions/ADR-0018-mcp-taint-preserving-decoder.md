# ADR-0018: MCP Transport 反序列化使用 TaintPreservingDecoder，禁用 encoding/json 直解

- **状态**: Accepted
- **日期**: 2026-05-21
- **决策者**: AI Architect
- **相关模块**: M7 Tool Action Layer / M11 Policy Safety (Taint Tracking 主防线)

## 上下文

MCP JSON-RPC 消息为动态嵌套结构（工具的输入/输出 schema 由 server 自定义，宿主侧无预定义 Go struct）。直觉做法是 `json.Unmarshal(body, &map[string]interface{}{})`，对返回值按字段取用。但这会将所有 string 字段降级为裸 Go `string` 类型，**丢失 M11 §2.1 Taint Tracking 主防线的 TaintLevel 标记**。

M11 §2.1 的第四重防线要求外部输入必须保持 `TaintedString` 类型并携带 source/origin 元数据，直至显式经 Sanitizer 降级；编译器+运行时双重保证 TaintedString 不可隐式注入 instruction slot。`map[string]interface{}` 解析会让此契约在 MCP transport 边界击穿。

## 决策

**MCP Transport 层使用 `TaintPreservingDecoder`，递归遍历动态 JSON 树并把每个 string 叶子包装为 `TaintedString`。禁止 MCP Client 路径上使用 `encoding/json` 直接解到 `map[string]interface{}`。**

包装规则：
- 所有 string 叶子 → `TaintedString{Source=MCP, Origin=server_name}`
- 初始 `[TaintLevel]` 按 M11 §2.4 `[Connector-Taint-Table]`：白名单 MCP → TaintMedium；其余 → TaintHigh
- number / bool / null 不包装，保留 JSON 路径以备追溯
- 与 `TaintedJSONNode` 共享污点包装逻辑

## 后果

- **正向**：闭合 M11 主防线在 MCP 边界的缺口；动态 schema 场景无需预定义 struct；ToolResult 返回 M4/M5/M10 前完成打标。
- **负向**：MCP path 上禁用标准 `encoding/json`，新增 reviewer 心智负担；TaintPreservingDecoder 实现需对 map / slice / nested struct 递归，性能略低于裸 unmarshal。
- **反例守护**：未来如有人提议「MCP 客户端某个 ad-hoc 字段读取走 `json.Unmarshal` 走捷径」——拒绝。物理边界一旦破例即作废防线。建议 CI lint 检测 `pkg/action/mcp_*` 文件中的 `encoding/json` import。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 默认 `map[string]interface{}` + 后置批量 retag | 后置 retag 无法恢复 source/origin 元信息；时序窗内可能泄漏到不被允许的 slot |
| 给每个 MCP server 预定义 Go struct + tag 注入 | 动态 schema 场景不可行；用户自定义工具天然变化快 |
| 在 prompt 装配层（M4）兜底 retag | 违反 fail-fast；M4 已是 instruction slot 边界，迟于此处即为防线穿透 |

## 引用代码

- `pkg/action/taint_preserving_decoder.go`（实现）
- `pkg/action/mcp_client.go`（接入点）
- `docs/arch/M07-Tool-Action-Layer.md §1`（MCP Transport 污点保护反序列化）
- `docs/arch/M11-Policy-Safety.md §2`（Taint Tracking 主防线 / §2.4 Connector-Taint-Table）
- `internal/protocol/taint.go`（TaintedString / TaintedJSONNode）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-21 | 初稿，从 M07 §1 抽离决策 |
