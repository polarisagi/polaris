# ADR-0111: 跨 Agent 与跨节点委派：出站 a2a_delegate、Polaris 间 MCP 门面、外部 A2A 1.0

> 状态：Accepted（第一、二期已落地；流式、`a2a-bridge`、跨节点 OTel/指标/audit 未做） ｜ 首次：2026-08-02（原 0084）/ 2026-10-12 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0084、原 ADR-0017 决策三
> 一句话：一个内部委派服务 + 两个协议门面（`/mcp` 给 Polaris 对端与上层调度者，`/a2a/v1` 给外部 Agent）；出站统一走 `MCPManager.CallTool`，内核不新增 A2A 出站客户端。

## 决策一：出站——外部/对端 Agent 建模为 MCP 工具 `a2a_delegate`，复用 `transfer_to_agent` 挂起通道（原 ADR-0084 决策 1–9）

触发事实：`transfer_to_agent` 的异步挂起通道（`agent_handoff:<role>` → `S_AWAIT_AGENT` → Blackboard 唤醒）原只有 `DefaultTaskWorker` 一个消费者，`target_agent_role` 语义被丢弃。

1. **寻址**：`target_agent_role = mcp:<server>/<agent>`；`server` 为 `MCPManager` 已连接实例名，`agent` 原样透传给对端 `a2a_delegate`，本地不解析。`Type: "agent_handoff:mcp:<server>/<agent>"` 由既有拼接天然产出。
2. **专用 Worker**：`MCPA2AWorker`（`internal/execute/orchestrator/mcp_a2a_worker.go`）自订阅 `task_posted` + CAS 认领，只认领 `agent_handoff:mcp:` 前缀；`DefaultTaskWorker` 以 `excludePrefixes` 前缀匹配排除。启动时补扫待认领任务。
3. **深度上限（A3）**：复用 `PostTask` 的 `SpawnDepth`/`resolveMaxDepth`（`inv_M8_06`，`state.yaml §m8_multiagent.delegation_chain_max_depth: 3`），不另设计数器。`StateContext.SpawnDepth` / `Agent.SetSpawnDepth` / `HeadlessOptions.WithSpawnDepth` 透传，`executeTransferToAgent` 投递取 `+1`（此前恒 0 等于未启用，对所有委派路径生效）。Worker 认领后调用前再比对一次（入口 + 执行前两道闸，防热更后遗留任务）。
4. **强制高污点（A1）**：`mcp:` 目标的 `ToolResult.TaintLevel` 经 `PropagateTaint(.., TaintHigh)`，only-up。
5. **PII 出境（A6）**：投递前对 `context_summary` 调 `PIIDetector.Redact`（不可逆）；detector 为 nil 记 Warn 放行（Tier-0 无 Presidio）。
6. **超时（A4）**：`CallTool` 套 `context.WithTimeout`，`A2AHandoffTimeoutSeconds`（默认 600s，`state.yaml §m8_multiagent.mcp_a2a_handoff_timeout_seconds`）。
7. **网络（A7）**：不新增客户端，`CallTool` 的 HTTP 传输已走 SafeDialer。
8. **发现**：`MCPManager.ListA2AAgents` 检测已连接 Server 是否暴露 `a2a_delegate`（有 `a2a_list_agents` 则取回 `MCPAgentDescriptor{Server,Agent,Description}`，否则 `{Agent:"default"}`）；LLM 侧经内置工具 `list_agents`（本地子 Agent + `mcp:` 目标 + `general-purpose`），`transfer_to_agent` 同注册（`RegisterDelegationTools`，原 `list_a2a_agents`/`RegisterA2ATools` 由 ADR-0103 决策三并入）。
9. **契约增补（本 ADR）**：`a2a_delegate` 入参在 `target_agent / context_summary / namespace` 外增 `spawn_depth`（int）与 `chain`（节点 ID 数组），由 `MCPA2AWorker.WithDelegation` 注入。`namespace` 仅为对端命名空间**提示**，对端一律加 `peer:<A>/…` 前缀，不得写入本地用户命名空间（HE-7）。
10. **Polaris → Polaris**：B 登记为普通 `[[mcp.servers]]`（B 的 `/mcp`，Streamable HTTP）；B 的 `a2a_delegate` 以 MCP Tasks 扩展任务返回（`resultType:"task"`），A 端 `awaitTask` 轮询，上限同 `A2AHandoffTimeoutSeconds`。外部非 Polaris A2A Agent 出站**本期不做**；将来只做独立 stdio MCP 子命令 `polaris a2a-bridge`（内部 `a2aclient`），内核零改动。
11. **契约工具令牌**：`MCPManager.SetDelegationTokenMinter` 仅为 `a2a_delegate`/`a2a_list_agents`/`get_task`/`cancel_task` 补发一次性 JIT 能力令牌（用后撤销，授权依据为上游 `transfer_to_agent` 已过 S_VALIDATE）；此前无令牌导致 trust_tier≤2 目标一律被执行闸门拒绝。

## 决策二：入站——A2A 1.0 门面 `/a2a/v1` 取代私有 `delegate_task`（原 ADR-0017 决策三入站部分，订正）

原 `POST /v1/a2a/tasks` 是私有协议：方法名 `delegate_task`、只回 `accepted`、取不回结果、任务 ID 取自调用方、`SpawnDepth` 恒 0、`TaintHigh` 字段不落库。

1. **SDK**：官方 `github.com/a2aproject/a2a-go/v2`（v2.6.0）`a2asrv`，仅 JSON-RPC 绑定，挂现有 mux（`internal/gateway/server/meshapi/`），不另起监听、不引入 `a2agrpc`（探针二进制 +0.86MB，CGO_ENABLED=0 可构建）。
2. **Agent Card**（`/.well-known/agent-card.json`，免鉴权）：`supportedInterfaces[]={url, protocolBinding:"JSONRPC", protocolVersion:"1.0"}`；`skills[]` 来自 `[[a2a.skills]]`；`securitySchemes` 声明 bearer；`pushNotifications:false`；`extendedAgentCard` 本期 false（`meshapi/card.go`）；不列内部工具。签名 `a2acrypto` JWS+JCS（RFC 7515/8785），范围不含 `signatures`；密钥为**独立的节点身份 Ed25519 密钥**，与能力令牌密钥分离（卡公钥需被对端固定，能力令牌密钥需独立轮换）。
3. **方法**：已实现 `SendMessage`（`returnImmediately`，按 `messageId`+调用方去重）、`GetTask`、`CancelTask`（映射黑板取消与会话 KillSwitch）、`ListTasks`（按调用方作用域过滤，游标分页）。未实现：`SendStreamingMessage`/`SubscribeToTask`、`GetExtendedAgentCard`。**任务 ID 一律本节点生成**，调用方 ID 仅作关联。
4. **状态映射**：

   | 黑板状态 | A2A | MCP Tasks |
   |---|---|---|
   | Pending | SUBMITTED | working |
   | Claimed/Executing | WORKING | working |
   | Completed / Failed / Canceled | COMPLETED / FAILED / CANCELED | completed / failed / cancelled |
   | Cedar/策略拒绝、depth/chain 拒绝 | REJECTED | failed |
   | Suspended（HITL） | INPUT_REQUIRED（仅状态，见决策五） | working + statusMessage（不发 `input_required`） |
   | 需额外授权 | AUTH_REQUIRED | — |

   时间戳 ISO 8601 UTC 毫秒。
5. **错误**：`google.rpc.Status` + `ErrorInfo{domain:"a2a-protocol.org"}`，`apperr` Code ↔ reason 映射。版本协商读 `A2A-Version`，不支持返回 VersionNotSupported；解析 `A2A-Extensions`。
6. **旧端点**：`POST /v1/a2a/tasks` 仅 `[a2a] legacy_endpoint=true`（默认 false）时注册，每次调用打 deprecation Warn；`A2AConfig.Enabled=false` 时两门面路由一律不注册。
7. **污点**：入站 Part 包装 `TaintedString`；`data` Part 走 `TaintPreservingDecoder`（ADR-0103 决策五）；`url` Part 不自动抓取；`PostTask` 写入 `intent_taint` 列；执行侧 TaintHigh 仍由 `AcquireHeadless` 兜底。

## 决策三：节点身份与鉴权——对端令牌与 admin 密钥分离

1. `ClientTypePeer`；令牌为 256 位随机不透明令牌（`pmt_` 前缀），库内只存 SHA-256（`mesh_peer_tokens`，`internal/protocol/schema/048_mesh.sql`）。**不用**能力令牌：TokenManager 签名密钥进程内临时生成、重启失效，且签发方即校验方时签名不比查表多提供安全，查表天然支持列表与吊销。scope ⊆ {a2a:send, a2a:read, a2a:cancel}。CLI `polaris peer invite|accept|issue|revoke|list`（走 HTTP，ADR-0096 决策一，`cmd/polaris/cli_peer.go`）。
2. **路由白名单在中间件层强制**：peer 只能访问 `/a2a/v1`、`/mcp`、`/.well-known/agent-card.json`，其余 403（`middleware_auth.go`）；`/mcp` 下只暴露 `a2a_delegate`/`a2a_list_agents`/`get_task`/`cancel_task`，不暴露本机工具目录。
3. Cedar principal `Peer::"<node-id>"`。对端任务以只读角色执行（`AgentProfileSpec.ReadOnly`，写类工具在执行入口硬拒绝；MCP 工具无能力等级元数据，一律视为可写，fail-closed；`[mesh] peer_read_only` 可关），默认禁止再委派（`allow_relay=false`，从源头杜绝成环）。**只读不等于不泄露**：`AgentProfileSpec.FileRoot` 将内置文件/命令工具限定到 `[mesh] peer_file_root`（默认 `<data_dir>/workspace/peer-shared`，替换而非追加白名单，根非法 fail-closed），否则 `read_file`/`data_query` 可读到 `vault.key`、`polaris.db`。越权计划在 S_VALIDATE 前段打回重规划，不落到污点闸门转本机 HITL。
4. 对端登记存库 `mesh_peers`（含固定 `card_pubkey`，不做 TOFU；拉取 Agent Card 校验签名不符即拒）；配对为 `polaris peer invite`/`accept` 两次复制粘贴（人即确认者），对方回邀请时才把预签发令牌绑定到真实节点 ID；重配对：邀请写明发给本节点且仍持有效令牌即视为已配对，新邀请作废未接受旧邀请令牌。对端 MCP Server 以 trust_tier 2 登记（结果仍 TaintHigh）；出站令牌存于该 MCP Server 的 `Authorization` 头。设置页「多节点」经 `GET/PUT /v1/mesh/settings` 写回用户 `config.toml`（`config.PatchUserConfig` 只改目标行，先 `Load` 校验再原子替换）。
5. 上层调度者（claude.ai 连接器）OAuth 不在本期；`/mcp` 只接受 peer 令牌。

## 决策四：防环——`spawn_depth` + `chain` 跨网络传递

1. A2A 门面：扩展 URI `https://polarisagi.online/a2a/ext/delegation/v1`，在 message metadata 携带；MCP 门面经 `a2a_delegate` 入参；共用一个校验函数（`internal/gateway/mesh/chain.go`）。
2. 入站：`spawn_depth` 写入 `entry.SpawnDepth`，超 `delegation_chain_max_depth` → REJECTED；本节点 ID 已在 `chain` → REJECTED；未带扩展按 depth=1、chain=[对端 sub]；`chain` 尾部以令牌 `sub` 为准防伪造链首。出站追加本节点 ID，depth+1。单一权威校验点仍为 `PostTask`（HE-2）。

## 决策五：HITL 只在执行节点本地完成

对端只见「待审批」状态；MCP 门面不发 `input_required`/`inputRequests`（A 端 MRTR 自动应答路径会被误用为远程审批）；A2A 门面返回 INPUT_REQUIRED 时不接受对端 `SendMessage` 续写完成审批。

## 决策六：监听与网络

1. `[interface].extra_listen = ["100.x.y.z:28888"]`；默认仍仅 loopback；拒绝 `0.0.0.0`/`::` 除非 `allow_wildcard_listen=true`（Warn）；非 loopback 监听上仅 peer 令牌与 `POLARIS_API_KEY` 有效，本机令牌豁免不适用。
2. SafeDialer 与 EgressGateway 域名白名单**均**只放行 `mesh_peers` 登记的精确 `host:port`，不放行整段 100.64.0.0/10。
3. Tailscale 不做专用集成（不内嵌 tsnet，不调 tailscale CLI）：它对 Polaris 只是三层网络，tsnet 会增约 10MB 并引入第二套身份体系；设置页仅只读枚举本机网卡给候选。
4. **已知偏差**：`ARCHITECTURE.md §1` 要求远程绑定带 TLS，本期 `extra_listen` 为明文 HTTP，依赖 WireGuard 链路加密，绑定非 Tailscale 地址时启动告警。

## 反例守护
- 禁止新增独立 A2A HTTP 客户端/JSON-RPC 协议栈用于 Polaris 间委派；外部 A2A 出站只能走 `a2a-bridge` 独立进程；不得以「A2A 已冻结」为由在内核新增（原 0084/0111）。
- 禁止 `mcp:` 结果 TaintLevel 低于 TaintHigh；禁止跳过 `context_summary` PII 脱敏；禁止为深度计数新增独立字段（`SpawnDepth` 为 SSoT）。
- 禁止 peer 令牌访问 `/a2a/v1`、`/mcp`、Agent Card 以外路由；禁止 peer 写入本地用户记忆命名空间；禁止调用方指定本节点任务主键。
- 禁止远程对端代替本地人类完成 HITL（含 MRTR `inputResponses` 与 A2A 续写两途径）；禁止整段放行 CGNAT 以通过 SSRF 防护。
- `scripts/rejected-proposals.txt` 的 `well-known`/`agent-card` 条目已指向本 ADR；旧「A2A 入站已实现」前提不成立，勿据此拦截。

## 已驳方案
| 方案 | 驳回理由 |
|---|---|
| 独立「任务委派接口层」不复用 `CallTool`（原 0017 决策三字面） | 两套外部通信客户端；Blackboard 异步包裹同步 `CallTool` 已满足 Tool/Task Layer 边界（0084） |
| 同步阻塞 `executeTransferToAgent` 等外部结果 | 倒退回独占 DAG 执行槽（0084） |
| 独立 `polaris mesh` 进程（本机 stdio MCP + A2A 客户端） | 多一进程多一跳；仅「调外部 A2A」有价值，保留为 `a2a-bridge`（0111） |
| 内核内置 A2A 出站（修订 0084） | 第二套交叉实现理由未因 SDK 稳定消失，MCP 门面已满足出站（0111） |
| OpenClaw 式 Hub + Node | 每台机器已是完整实例（ADR-0096）；Node 能力可作 `/mcp` 后续工具类，另立 ADR（0111） |
| NATS/Redis；跨节点同步数据库 | 违反单二进制与 State-in-DB；违反记忆隔离 HE-7（0111） |
| 入站继续自研 A2A 栈 | 官方 SDK <1MB 无 CGO，自维护成本更高（0111） |

## 重新评估触发条件
1. 需调用非 Polaris A2A Agent 且频率抵消子进程成本 → 启用 `a2a-bridge`；MCP 出站无法承载 A2A 流式语义时再议内核内置出站。
2. 场景转为「一个大脑 + 远程手脚」→ 另立 ADR 引入 Node 工具类。
3. MCP/A2A 发布不兼容新主版本，或 claude.ai 连接器认证方式变化；出现不经 Tailscale 的跨机部署 → 节点身份密钥签发自签证书 + 固定公钥，补 TLS 监听。
4. 非本 ADR 范围：MCP 工具能力等级缺省 CapReadOnly 使 S_VALIDATE 污点闸门对 MCP 工具放行（全局问题，另立 ADR）。

## 沿革
| 日期 | 变更 | 原编号/理由 |
|---|---|---|
| 2026-05-21 | A2A v0.3 入站私有端点；决策三「出站不复用 M7 MCP 客户端、新增任务委派接口层」 | 原 0017 决策三（出站部分 2026-08-02 被 0084 推翻；入站被决策二取代） |
| 2026-08-02 | 出站 A1~A7 落地 | 原 0084 |
| 2026-09-26 | `list_a2a_agents` 并入 `list_agents`；本地角色路由缺口由 ADR-0103 解决（`DefaultTaskWorker.WithProfileResolver`） | 原 0084 追记 |
| 2026-10-12 | A2A 1.0 + MCP 门面设计，用户审阅转 Accepted | 原 0111 |
| 2026-10-12 | 实现落地偏差：不透明令牌替代能力令牌；只读角色替代转 HITL；对端登记入库；`extendedAgentCard=false`；`PostTask` 补写 `intent_taint` | 原 0111 |
| 2026-10-12 | 双实例实测修复：Egress 白名单、契约工具令牌、S_EXECUTE→S_AWAIT_AGENT 转移零效应（cd93d66 回归，现产出 DeterministicEffect，`fsm/handoff_effects_test.go` 守护）、FileRoot、启动补扫、`/v1/mcp-servers` MCPManager 回填 | 原 0111 / 0084 追记 |
| 2026-10-12 | 设置页「多节点」落地 | 原 0111 |

## 引用代码
- `internal/gateway/server/meshapi/`（`a2a.go`、`card.go`、`mcp.go`、`principal.go`、`routes.go`）、`internal/gateway/mesh/`、`internal/gateway/server/server_mesh.go`、`internal/gateway/server/sysadmin/a2a/admin_a2a.go`（旧端点）
- `internal/gateway/server/server_routes.go`、`internal/gateway/server/middleware_auth.go`
- `internal/execute/orchestrator/mcp_a2a_worker.go`、`internal/execute/orchestrator/default_worker.go`
- `internal/extension/mcp/agent_descriptor.go`、`internal/extension/mcp/mcp_client_tasks.go`、`internal/extension/mcp/mcp_manager_tools.go`
- `internal/tool/builtin/delegation_tools.go`、`internal/tool/builtin/list_agents.go`
- `internal/agent/agent_handoff.go`、`internal/agent/pool.go`、`internal/agent/fsm/state_machine.go`
- `internal/security/network/safe_dialer.go`、`internal/store/repo/repo_mesh.go`、`internal/protocol/schema/048_mesh.sql`、`cmd/polaris/cli_peer.go`
- `docs/arch/spec/state.yaml §m8_multiagent`、`docs/arch/M07-Tool-Action-Layer.md`、`docs/arch/M08-Multi-Agent-Orchestrator.md`
