# ADR-0017: MCP 传输层与协同架构合集（Streamable HTTP + TaintPreservingDecoder + A2A 战略方向，含原 ADR-0018/0070）

- **状态**: Accepted（决策一/二已执行）/ Proposed（决策三，战略方向，未排期）| **日期**: 2026-05-21（合并 2026-07-28）| **模块**: M7 `internal/extension/mcp/` / M11

## 决策一：默认传输层选用 Streamable HTTP，SSE 降级为 legacy（原决策）

Streamable HTTP 为默认远程传输层（2025-11-25 MCP spec 强制要求），SSE 仅向后兼容旧 server、标注 legacy；stdio 仅用于本地子进程 MCP server。新接入 server 必须走 Streamable HTTP。

## 决策二：Transport 反序列化使用 TaintPreservingDecoder（原 ADR-0018）

MCP JSON-RPC 动态嵌套结构禁止用 `encoding/json` 直解到 `map[string]interface{}`（会丢失 M11 §2.1 Taint Tracking 主防线的 TaintLevel 标记）。改用 `TaintPreservingDecoder` 递归遍历，所有 string 叶子包装为 `TaintedString{Source=MCP, Origin=server_name}`：白名单 MCP → TaintMedium，其余 → TaintHigh。

## 决策三：MCP Agent-to-Agent（A2A）协同架构v0.3（原 ADR-0070，部分落码）

现有 MCP 客户端仅停留在"调用函数"级别工具层交互，缺乏异步任务声明/状态跟踪/结果回调语义。现确立设计思路并实现基础入站端点：

- **入站（已实现）**：`internal/gateway/server/sysadmin/a2a` 新增 `GET /.well-known/agent-card.json` (暴露 Agent 能力) 和 `POST /v1/a2a/tasks` (接收 JSON-RPC 2.0 任务)。请求封装为 `TaskEntry`，强制置为 `TaintHigh`，落入 `sqlite_blackboard`，经 M11 策略体系（污点传播+Cedar 门控）拦截，与 Channel 消息同等对待。
- **出站（待实现）**：不复用 M7 MCP 客户端 `ExecuteTool` 路径，新增"任务委派"接口层，按信任模型判断被委派 Agent 的沙盒信任等级。
- **边界**：Tool Layer（M7 MCP，低延迟同步）与 Task Layer（A2A MCP，异步容错）实现完全隔离，避免超时/锁死混淆。
- **依赖**：[ADR-0062](./ADR-0062-deadcode-final-settlement.md)（含原 ADR-0050 自订阅 CAS 决策）/ [ADR-0016](./ADR-0016-unified-trust-extension-model.md)（信任模型）。

## 反例守护

拒绝新 MCP server 用 SSE 实现"以简化"——SSE 仅用于兼容旧 server。拒绝任何 MCP 客户端字段读取走 `json.Unmarshal` 抄近路——物理边界一旦破例即作废防线。

## 引用代码

`internal/extension/mcp/mcp_client.go`、`internal/extension/mcp/taint_decoder.go`、`internal/security/taint/taint.go`、`internal/gateway/server/sysadmin/a2a/admin_a2a.go`、`internal/gateway/server/server_routes.go`
