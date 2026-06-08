# ADR-0017: MCP 默认传输层选用 Streamable HTTP，SSE 降级为 legacy

- **状态**: Accepted
- **日期**: 2026-05-21
- **决策者**: AI Architect
- **相关模块**: M7 Tool Action Layer (pkg/action/mcp)

## 上下文

MCP 协议早期定义了 stdio / SSE（Server-Sent Events）/ HTTP 三类传输层。2025-11-25 spec 升级强制要求实现方支持 **Streamable HTTP**（基于 HTTP 长连接 + 增量 chunked 推送），并将 SSE 标注为 legacy。Polaris MCP Client/Server 需要在传输层做单点选型，避免出现"两套都实现 + 调用方按 Provider 切换"的复杂分支。

## 决策

**Streamable HTTP 为默认远程传输层。SSE 保留仅向后兼容，标注 legacy。**

- 新接入的 MCP server：必须经 Streamable HTTP；客户端遇到只支持 SSE 的旧 server 才走 legacy 路径
- 错误映射表（M07 §1）中两者并存，但 Streamable HTTP 行优先级 > SSE
- stdio 仅用于本地子进程 MCP server（无远程网络层）

## 后果

- **正向**：与 MCP spec 2025-11-25+ 对齐，享受官方维护；HTTP 反向代理、负载均衡、CDN 等基础设施直接复用；HTTP/2 多路复用消除 SSE 单连接瓶颈。
- **负向**：增量推送语义比 SSE 略复杂（需处理 chunked + close frame）；遗留 SSE server 必须双栈维护至少 12 个月。
- **反例守护**：未来如有人提议「新 MCP server 用 SSE 实现以简化」——拒绝。SSE 仅用于兼容旧 server，新建一律 Streamable HTTP。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| SSE 作为默认 | 2025-11-25 spec 明确 legacy；单连接瓶颈；缺 close frame 致连接状态歧义 |
| stdio 作为远程默认 | 仅适用本地子进程，远程不可用 |
| 双默认（按 Provider 探测） | 增加 Provider 注册复杂度；调用方需关心传输层，违反封装 |

## 引用代码

- `pkg/action/mcp_client.go`（Streamable HTTP 实现）
- `pkg/action/mcp_transport.go`（错误映射）
- `docs/arch/M07-Tool-Action-Layer.md §1`（MCP 双向架构）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-21 | 初稿，从 M07 §1 抽离决策 |
