# ADR-0084: MCP A2A —— 复用 transfer_to_agent 挂起机制补齐出站跨框架委派

- **状态**: Accepted（已执行）
- **日期**: 2026-08-02
- **决策者**: 系统架构师
- **相关模块**: M8 `internal/execute/orchestrator/mcp_a2a_worker.go`、M7 `internal/extension/mcp/agent_descriptor.go`、M4 `internal/agent/agent_handoff.go`、`internal/tool/builtin/list_a2a_agents*.go`

## 上下文

ADR-0017 决策三已实现 A2A v0.3 入站端点（`GET /.well-known/agent-card.json` + `POST /v1/a2a/tasks`），出站方向标注"待实现"。`transfer_to_agent` 工具（D5/GD-1）已有一条成熟的异步挂起委派通道：投递 `agent_handoff:<role>` 任务 → `S_AWAIT_AGENT` 挂起 → Blackboard 事件订阅唤醒 → 恢复。但复核确认该通道当前只有一个消费者——`DefaultTaskWorker`（`internal/execute/orchestrator/default_worker.go`），它把 `Intent` 原样当纯文本 headless 查询执行，完全丢弃 `target_agent_role` 的语义、`Namespace`、`SpawnDepth`；M8 Orchestrator 的中心化按角色下推机制（`RegisterWorker`）在生产环境从未激活。换言之，`target_agent_role` 参数迄今是纯装饰性字段。

本 ADR 是 ADR-0017 决策三"出站待实现"的补齐，但不是从零新建协议客户端：外部 Agent 建模为一种特殊的 MCP 工具（已连接的 MCP Server 暴露约定名 `a2a_delegate`/`a2a_list_agents` 工具），复用 `MCPManager.CallTool` 完成实际网络调用，复用 `transfer_to_agent` 现有异步挂起/唤醒/崩溃恢复机制完成任务生命周期——是入站方向的另一翼，不重复实现入站，也不新建第二套委派协议栈。

## 决策

1. **寻址约定**：`target_agent_role` 支持 `mcp:<server>/<agent>` 前缀。`server` 对应 `MCPManager` 已连接实例的 LLM 侧服务器名，`agent` 是目标 Server 内部路由到的子 Agent 标识，原样透传给该 Server 的 `a2a_delegate` 工具，不在本地解析语义。`executeTransferToAgent` 对该前缀零特殊分支——`Type: "agent_handoff:" + targetRole` 的既有拼接逻辑天然产出 `"agent_handoff:mcp:<server>/<agent>"`，无需改动投递路径本身。
2. **专用 Worker 拦截**：新增 `internal/execute/orchestrator/mcp_a2a_worker.go`（`MCPA2AWorker`），复用 `DefaultTaskWorker`/`DebateWorker` 的"自订阅 `task_posted` + CAS 认领"idiom，仅认领 `strings.HasPrefix(snap.Type, "agent_handoff:mcp:")` 的任务。`DefaultTaskWorker.tryClaimAndExecute` 的排除判定由精确匹配改为前缀匹配（`excludePrefixes`），使 `"agent_handoff:mcp:"` 可作为前缀条目排除，不再被通用兜底误当纯文本 headless 查询执行——这是 `target_agent_role` 第一次被赋予真实运行时语义。
3. **深度上限（A3）**：复用既有 `SQLiteBlackboard.PostTask` 的 `SpawnDepth`/`resolveMaxDepth` 校验（`inv_M8_06`，`state.yaml §m8_multiagent.delegation_chain_max_depth: 3`），不新增独立计数器。修复该机制此前对 `transfer_to_agent` 委派链完全失效的缺口——`executeTransferToAgent` 从未设置 `entry.SpawnDepth`（恒为 0，永远小于上限，等同未启用）：新增 `fsm.StateContext.SpawnDepth`、`Agent.SetSpawnDepth`、`types.HeadlessOptions.SpawnDepth`/`WithSpawnDepth`，由 `AcquireHeadless` 调用方（`DefaultTaskWorker`/`MCPA2AWorker` 未来若有嵌套委派）透传 `snap.SpawnDepth`，`executeTransferToAgent` posting 时取 `a.sCtx.SpawnDepth + 1`。单一权威校验点仍是 `PostTask`（HE-2 可验证执行不重复实现）。
4. **强制高污点（A1）**：`executeTransferToAgent` 恢复分支检测到 `targetRole` 带 `mcp:` 前缀时，对返回给 DAG 执行器的 `ToolResult.TaintLevel` 做 `types.PropagateTaint(taintLevel, types.TaintHigh)`——跨框架外部 Agent 的产出永远不信任，不因发起方自身会话污点更低而降级（only-up）。
5. **PII 脱敏出境（A6）**：`targetRole` 带 `mcp:` 前缀时，投递任务前对 `context_summary` 调用 `a.Security.PIIDetector.Redact`（静态不可逆脱敏，非 `RedactWithMode` 的可逆令牌化——离开信任边界的内容不应留有可被外部方复原的映射）；`PIIDetector` 为 nil 时记录 Warn 级日志后放行（与既有 headless 路径 PII 降级策略一致，Tier-0 无 Presidio 场景不阻断主流程）。
6. **超时（A4）**：`MCPA2AWorker` 对 `MCPManager.CallTool` 调用施加 `context.WithTimeout`，上限取新增阈值 `M8OrchestratorThresholds.A2AHandoffTimeoutSeconds`（默认 600s / 10min，`state.yaml §m8_multiagent.mcp_a2a_handoff_timeout_seconds`）。
7. **网络出口（A7）**：不新增网络客户端——`MCPManager.CallTool` 走的 HTTP 传输已经过 `sb.SafeHTTP`（SafeDialer），无需重复接入。
8. **发现（A2/A5 的可发现性前提）**：新增 `MCPManager.ListA2AAgents(ctx)`，遍历已连接 Server，检测其工具列表中是否含 `a2a_delegate`（A2A 能力标记）；若同时暴露 `a2a_list_agents` 则调用取回 `[]MCPAgentDescriptor{Server, Agent, Description}`，否则退化为单条 `{Agent:"default"}`。新增内置工具 `list_a2a_agents`（`internal/tool/builtin/list_a2a_agents*.go`）把该列表暴露给 LLM——这是本次复核过程中发现的更大缺口（`transfer_to_agent`/`code_act` 作为 DAG 节点 action 名从未在任何工具目录/Prompt 模板中向 LLM 暴露过，全仓搜索确认无命中）的一个局部但立即可用的止损：至少 `mcp:` 前缀合法目标现在有一条真实可调用的发现路径，不依赖那个未定位到的隐藏机制。完整定位并修复该隐藏机制留给阶段06（见 `local_playground/upgrade/99-new-findings.md`）。
9. **执行前二次校验（防御性 HE-2）**：`MCPA2AWorker` 认领任务后，即便 `PostTask` 时的深度校验已通过，仍在发起外部调用前重新读取 `snap.SpawnDepth` 与全局上限比对；超限直接 `FailTask`，不发起外部网络调用——入口 + 执行前两道闸，非重复实现（enforcement 逻辑相同，触发时机不同：前者防止任务被写入黑板，后者防止极端场景下已写入但尚未认领的任务在配置热更后仍被执行）。

## 后果

- **正向**：`target_agent_role` 从"投递即忘、内容语义全部丢失"变为对 `mcp:` 目标有真实路由+超时+污点+PII 出境控制的委派通道；`SpawnDepth` 传递缺口一并修复，惠及所有 `transfer_to_agent` 调用（不限于 `mcp:` 路径）。
- **负向**：非 `mcp:` 前缀的本地角色委派（如 `librarian`/`governance`）仍然只能落到 `DefaultTaskWorker` 的纯文本 headless 兜底——`NamespaceID` 在 `AcquireHeadless` 路径依旧不透传（`agent.SetMemoryNamespace` 从未在该路径被调用），角色本身仍不路由到差异化 Persona。这是比本 ADR 范围更大的既有缺口（详见"被驳回的方案"），登记于 `99-new-findings.md`，不在本次修复范围。
- **已知限制**：`SpawnDepth` 经 `HeadlessOptions` 只透传一层（`DefaultTaskWorker`→`AcquireHeadless`→`Agent.SetSpawnDepth`）；若某个被委派的 headless Agent 自身再次调用 `transfer_to_agent`，其 `a.sCtx.SpawnDepth` 已正确递增（因为透传链路已补齐），因此链式委派的深度上限对所有路径（含非 `mcp:`）现在都是生效的，不存在"只修 mcp: 路径"的不对称。

## 反例守护

- 禁止新增独立的 A2A HTTP 客户端/JSON-RPC 协议栈——出站必须复用 `MCPManager.CallTool`，把外部 Agent 当作 MCP 工具的一个特殊类别，而非平行协议实现（否则与 ADR-0017 决策三"Tool Layer 与 Task Layer 隔离"的边界产生第二套交叉实现）。
- 禁止在 `MCPA2AWorker` 里把外部 Agent 结果的 `TaintLevel` 降到 `TaintHigh` 以下。
- 禁止跳过 `context_summary` 的 PII 脱敏直接投递（`mcp:` 前缀分支）。
- 禁止在本 ADR 范围内"顺手"修复 `NamespaceID`/角色路由对 `DefaultTaskWorker` 路径的缺口——那是独立的、影响面更广的既有 bug，需要单独评审（会牵动 `Intent`/`HeadlessOptions` 的进一步扩展与向后兼容策略）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 新建独立"任务委派接口层"（不复用 `MCPManager.CallTool`），完全对齐 ADR-0017 决策三原文措辞 | 会形成两套外部通信客户端（M7 MCP 工具调用 + 新客户端），违反"最少代码集"；用 Blackboard 异步任务包裹同步 `CallTool` 已经满足决策三"Tool Layer 低延迟同步 / Task Layer 异步容错"的架构边界诉求，无需额外协议栈 |
| 同步阻塞 `executeTransferToAgent` 等待外部 A2A 调用完成 | 与 GD-1 已确立的异步挂起设计倒退，重新引入独占 DAG 执行槽位问题 |
| 顺带修复 `NamespaceID`/角色路由对本地非 `mcp:` 委派的缺口 | 影响面覆盖 `Intent`/`HeadlessOptions`/`AcquireHeadless` 全链路，超出本 ADR 的 A2A 出站范围，需独立 ADR 评审 |
| 为深度计数新增独立字段（不复用 `SpawnDepth`） | `SpawnDepth`/`resolveMaxDepth`/`MaxSpawnDepth=3` 已是 `state.yaml` 登记的 SSoT（`delegation_chain_max_depth`），新增字段会制造语义重复的第二个真值来源 |

## 引用代码

- `internal/agent/agent_handoff.go`（`executeTransferToAgent`）
- `internal/execute/orchestrator/mcp_a2a_worker.go`（新增）
- `internal/execute/orchestrator/default_worker.go`（`excludePrefixes` 改造）
- `internal/execute/orchestrator/sqlite_blackboard.go`（`SpawnDepth`/`resolveMaxDepth`，复用不改动语义）
- `internal/extension/mcp/agent_descriptor.go`（新增，`MCPAgentDescriptor`/`ListA2AAgents`）
- `internal/tool/builtin/list_a2a_agents.go` / `list_a2a_agents_exec.go`（新增）
- `internal/agent/fsm/state_machine.go`（`StateContext.SpawnDepth`）
- `internal/agent/agent_wiring.go`（`SetSpawnDepth`）
- `internal/agent/pool.go`（`AcquireHeadless` 透传）
- `pkg/types/models_headless.go`（`HeadlessOptions.SpawnDepth`/`WithSpawnDepth`）
- `internal/config/thresholds.go §M8OrchestratorThresholds`（`A2AHandoffTimeoutSeconds`）
- `docs/arch/spec/state.yaml §m8_multiagent`（`mcp_a2a_handoff_timeout_seconds`）
- `docs/arch/decisions/ADR-0017-mcp-streamable-http-default.md`（决策三，本 ADR 的出站补齐对象）

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-08-02 | 初稿，随阶段05 P-03 落地 |
| 2026-08-09 | 追记：重新评估触发条件——`NamespaceID`/角色路由对 `DefaultTaskWorker` 本地非 `mcp:` 委派路径的缺口是独立于本 ADR 的既有 bug（登记于 `99-new-findings.md`），须单独 ADR 评审，不得在本 ADR 范围内顺手修复；新增独立 A2A HTTP 客户端的提议须先证明 `MCPManager.CallTool` 复用路径已无法满足需求。 |
