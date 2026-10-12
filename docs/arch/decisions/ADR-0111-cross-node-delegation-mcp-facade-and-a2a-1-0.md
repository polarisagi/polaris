# ADR-0111: 跨节点委派——Polaris 之间走 MCP 门面，外部互通走 A2A 1.0 入站

- **状态**: Accepted
- **日期**: 2026-10-12
- **决策者**: 用户（2026-10-12 审阅通过）/ Opus（调研、设计与实现）
- **相关模块**: M7 `internal/extension/mcp/`（新增服务端）、M8 `internal/execute/orchestrator/`、M11 `internal/security/{token,network}/`、M13 `internal/gateway/server/`、`cmd/polaris/`
- **修订**: ADR-0017 决策三（入站协议与状态行事实订正）、ADR-0084（`a2a_delegate` 契约增补 `spawn_depth` / `chain`，A1~A7 不变）

## 上下文

Polaris 要在多台机器上运行（Mac / 台式机 / VPS，经 Tailscale 组网，地址段 100.64.0.0/10），并互相委派任务；同时要让外部 Agent 以标准协议接入。阶段 0 复核（`local_playground/a2a-interop/stage0-facts.md`）确认了以下几点：

- 现有入站端点是私有协议：方法名 `delegate_task`，只回 `accepted`，调用方取不回结果，任务 ID 直接用调用方传来的 JSON-RPC `id`。
- `SpawnDepth` 不跨网络传递。
- Polaris 自身不是 MCP Server。
- 对端只能拿 admin 级的 `POLARIS_API_KEY`。
- SafeDialer 拦截 CGNAT 段。

有一条复核结论直接决定了方案：

- 出站契约 ADR-0084 已经落地：调用对端 MCP Server 的 `a2a_delegate` 工具。
- 自研 MCP 客户端已经对齐 2026-07-28：无状态新纪元、Tasks 扩展（`tasks/get` / `tasks/update` / `tasks/cancel`）、MRTR、静态 `Authorization` 头 + OAuth tokenSource。

也就是说，**Polaris → Polaris 委派在客户端一侧已经完整，缺的只是对端的 MCP 服务端**。

行业参照（2026-10）：

- OpenClaw 分两层：
  - 同一主人的多台机器用 Gateway + Node。Node 主动连接 Gateway，须人工配对批准；记忆、会话、审批只在 Gateway 一处。
  - 与外部 Agent 互通是独立的 A2A 1.0 channel 插件。只支持 JSON-RPC 绑定和文本，按对端独立 bearer token 鉴权，按对端隔离会话，CancelTask 拒绝。
- A2A v1.0 官方 Go SDK `github.com/a2aproject/a2a-go/v2` 当前为 v2.6.0。

## 决策

**一句话：一个内部委派服务，配两个协议门面。**

- **MCP 门面 `/mcp`**：给 Polaris 对端和 Claude 等上层调度者用。复用 ADR-0084 契约，出站零新代码。
- **A2A 1.0 门面 `/a2a/v1`**：给外部 Agent 用，基于官方 SDK `a2asrv`。

不新建 mesh 进程，也不在内核新增 A2A 出站客户端。

### 决策一：出站实现位置——驳回提示词选项 (a)/(b)，改为「对端暴露 MCP 门面」

1. Polaris A → Polaris B 的路径：`transfer_to_agent target="mcp:<B>/<agent>"` → `MCPA2AWorker` → `MCPManager.CallTool("a2a_delegate")` → B 的 `/mcp`（Streamable HTTP，无状态新纪元）。
   - A 的配置只需把 B 登记为一个普通 `[[mcp.servers]]`，url 指向 B 的 Tailscale 地址，`Authorization` 填对端令牌（见决策三）。
   - ADR-0084 的 A1~A7 原样生效：TaintHigh、PII 脱敏出境、超时、经 SafeDialer。
2. B 的 `a2a_delegate` 作为 MCP Tasks 扩展任务返回（`resultType:"task"`）。A 端已有的 `awaitTask` 负责轮询，超时上限沿用 `A2AHandoffTimeoutSeconds`，不阻塞 HTTP 连接。
3. `a2a_delegate` 入参在 `target_agent / context_summary / namespace` 之外新增 `spawn_depth`（int）与 `chain`（节点 ID 数组），`MCPA2AWorker` 同步透传。
   - `namespace` 只是对端命名空间的**提示**：B 端一律加 peer 前缀（`peer:<A>/…`），不得写入 B 的本地用户命名空间（记忆隔离，HE-7）。
4. 外部 A2A Agent 的出站（Polaris → 非 Polaris 的 A2A Agent）**本期不做**。如果将来需要，做成独立 stdio MCP Server 子命令 `polaris a2a-bridge`，内部使用 `a2aclient`。这是提示词选项 (a) 唯一保留的形态，内核仍然零改动。

### 决策二：入站协议——A2A 1.0 替换私有 `delegate_task`

1. **SDK 选型**：采用 `a2a-go/v2` 的 `a2asrv`，第一批只挂 JSON-RPC 绑定；`a2agrpc` 不引入。挂到现有 mux，不另起监听。
   - 实测增量：探针二进制 −s −w 后 +0.86MB，CGO_ENABLED=0 可构建，JSON-RPC 路径不链接 grpc。
   - 版本：SDK 要求 go 1.26.0，仓库为 1.26.2。
2. **Agent Card**（`/.well-known/agent-card.json`，免鉴权）：
   - `supportedInterfaces[]` 填 `{url, protocolBinding:"JSONRPC", protocolVersion:"1.0"}`。
   - `skills[]` 为结构化对象，来源于配置 `[[a2a.skills]]`。
   - `securitySchemes` 声明 bearer；`capabilities.extendedAgentCard:true`，`capabilities.pushNotifications:false`。
   - 公开卡不列内部工具清单。
   - 签名用 `a2acrypto`：JWS + JCS（RFC 7515 / RFC 8785），签名范围不含 `signatures` 字段本身。
   - 密钥为**独立的节点身份 Ed25519 密钥**，存 Vault，与 `internal/security/token` 的能力令牌签发密钥分离。理由：卡签名公钥要发给对端固定下来，能力令牌密钥要能独立轮换，两者生命周期不同。
3. **方法**：
   - 第一批：`SendMessage`（支持 `returnImmediately`，按 `messageId` + 调用方去重）、`GetTask`、`CancelTask`、`ListTasks`（按调用方作用域过滤，游标分页）、`GetExtendedAgentCard`。
   - 第二批：`SendStreamingMessage` / `SubscribeToTask`。
   - `CancelTask` 映射到黑板取消与会话级 KillSwitch。
   - **任务 ID 一律由本节点生成**，调用方 ID 只作关联字段。
4. **状态映射**（代码注释与 M08 同步）：

   | 黑板状态 | A2A 状态 |
   |---|---|
   | Pending | `TASK_STATE_SUBMITTED` |
   | Claimed / Executing | `TASK_STATE_WORKING` |
   | Completed | `TASK_STATE_COMPLETED` |
   | Failed | `TASK_STATE_FAILED` |
   | Canceled | `TASK_STATE_CANCELED` |
   | Cedar / 策略拒绝、depth / chain 拒绝 | `TASK_STATE_REJECTED` |
   | Suspended（HITL 待审批） | `TASK_STATE_INPUT_REQUIRED`（仅状态，见决策五） |
   | 需额外授权 | `TASK_STATE_AUTH_REQUIRED` |

   时间戳统一为 ISO 8601 UTC 毫秒。MCP 门面同一张表映射到 Tasks 扩展的 `working / completed / failed / cancelled`；HITL 待审批映射为 `working` + statusMessage，**不发 `input_required`**，见决策五。
5. **错误**：使用 `google.rpc.Status` + `ErrorInfo`（`domain:"a2a-protocol.org"`，`reason` 为大写下划线）。`apperr` Code ↔ reason 映射表放 `internal/gateway/server/a2a/errors.go`。
6. **版本协商**：读 `A2A-Version` 请求头，不支持时返回 VersionNotSupported；解析 `A2A-Extensions`。
7. **旧端点**：`POST /v1/a2a/tasks` 保留一个版本，受 `[a2a] legacy_endpoint`（默认 false）控制，每次调用打 deprecation Warn。
   - 同时修复 `A2AConfig.Enabled` 无人读取的问题：`enabled=false` 时两个门面的路由一律不注册。
8. **污点**：入站 Part 一律包装为 `TaintedString`；`data` Part 走 `TaintPreservingDecoder`；`url` Part 不自动抓取。
   - 改为由 `PostTask` 真正写入 `intent_taint` 列，补齐当前「字段设置了但不落库、无人读取」的缺口。
   - 实际执行时的 TaintHigh 仍然由 `AcquireHeadless` 兜底。

### 决策三：节点身份与鉴权——对端令牌与 admin 密钥分离

1. 新增 `ClientTypePeer`。
   - 令牌：由 `internal/security/token` 签发的 Ed25519 能力令牌，字段至少含 `sub`（对端节点 ID）、`aud`（本节点 ID）、`scope ⊆ {a2a:send, a2a:read, a2a:cancel}`、`exp`、`jti`。
   - 吊销表落库。
   - CLI：`polaris peer issue|revoke|list`，走 HTTP，遵守 ADR-0096 决策一。
2. **路由白名单在中间件层强制**：peer 身份只能访问 `/a2a/v1`、`/mcp`、`/.well-known/agent-card.json`，其他任何路由返回 403。`/mcp` 下 peer 只能看到、调用委派类工具（`a2a_delegate` / `a2a_list_agents`），不暴露本机工具目录。
3. Cedar principal 为 `Peer::"<node-id>"`。入站委派默认只读能力；写文件、执行 shell、联网写操作一律转 HITL。可按节点配置差异化策略。
4. 上层调度者（claude.ai 自定义连接器）的 OAuth 不在本期范围。到阶段 4 先查清 claude.ai 当前支持的认证方式（CIMD / DCR）再定。本期 `/mcp` 只接受 peer 令牌。

### 决策四：防环——`spawn_depth` + `chain` 跨网络传递

1. A2A 门面：定义扩展 URI `https://polarisagi.online/a2a/ext/delegation/v1`，在 message metadata 中携带 `spawn_depth` 与 `chain`。MCP 门面：通过 `a2a_delegate` 入参携带。两个门面共用一个校验函数。
2. 入站规则：
   - `spawn_depth` 写入 `entry.SpawnDepth`，超出 `delegation_chain_max_depth` 直接 REJECTED。
   - 本节点 ID 已在 `chain` 中，直接 REJECTED。
   - 调用方未带扩展时，按 depth=1、chain=[对端 sub] 处理。
   - `chain` 以令牌的 `sub` 为准做尾部校验，防止对端伪造链首。
3. 出站时 `chain` 追加本节点 ID，`spawn_depth` 取当前值 +1。单一权威校验点仍然是 `PostTask`（HE-2）。

### 决策五：HITL 只在执行节点本地完成

对端只能看到「待审批」状态，审批只能由执行节点的本地人类通过 Polaris 已有渠道完成。MCP 门面不对审批发 `input_required` / `inputRequests`，原因是 A 端客户端存在 MRTR 自动应答路径，会被误用为远程审批。A2A 门面返回 INPUT_REQUIRED 时，不接受对端用 `SendMessage` 续写来完成审批。

### 决策六：监听与网络

1. `[interface]` 新增 `extra_listen = ["100.x.y.z:28888"]`。
   - 默认仍只监听 loopback；非 loopback 地址必须显式配置。
   - 拒绝 `0.0.0.0` / `::`，除非 `allow_wildcard_listen=true`，开启时打 Warn。
   - 非 loopback 监听上，只有 peer 令牌和 `POLARIS_API_KEY` 有效，本机令牌豁免不适用。
2. SafeDialer：只对 `[[mesh.peers]]` 中登记的 `host:port` 精确放行，不放行整段 CGNAT。
3. 对端注册表 `[[mesh.peers]]`：`name`、`node_id`、`card_url`、`mcp_url`、`token_ref`（Vault）、`card_pubkey`（固定）。
   - 拉取 Agent Card 时校验签名，不匹配直接拒绝，不做 TOFU。
   - 登记后自动生成对应的 `[[mcp.servers]]`，使用者不需要配两遍。

## 后果

- **正向**：
  - Polaris 之间的委派复用已经验证过的出站链路，客户端与 Worker 零新代码。
  - 一个 `/mcp` 实现同时修正 README「支持作为服务端」的不实描述，并为 Claude 预留入口。
  - A2A 1.0 用官方 SDK，不自维护协议栈；能和 a2a-cli、A2A Inspector、OpenClaw A2A channel 互通。
  - 对端权限从 admin 收窄为三个 scope。
- **负向**：
  - 需要自研一个最小 MCP 服务端：`server/discover`、`tools/list`、`tools/call`、Tasks 扩展的 `tasks/get|update|cancel`。官方 `modelcontextprotocol/go-sdk` v1.8.0 对 2026-07-28 无状态服务端的支持未核实，阶段 2 前评估，不达标就自研。
  - 新增两类密钥：节点身份密钥、对端令牌吊销表。
  - 每个节点各有一套记忆与审批，跨机任务的审批要在执行节点完成。
- **反例守护**：
  - 禁止 peer 令牌访问 `/a2a/v1`、`/mcp`、Agent Card 以外的路由。
  - 禁止外部结果的 TaintLevel 低于 TaintHigh。
  - 禁止远程对端代替本地人类完成 HITL 审批，包括经 MRTR `inputResponses` 和 A2A 续写消息两种途径。
  - 禁止为通过 SSRF 防护而整段放行 100.64.0.0/10。
  - 禁止调用方指定本节点任务主键。
  - 禁止 peer 写入本地用户记忆命名空间。
  - 禁止以「A2A 已冻结」为由在内核新增 A2A 出站客户端：Polaris 之间已经走 MCP，外部 A2A 出站只能走 `a2a-bridge` 独立进程。
- **门控与文档联动**（落码时同步完成）：
  - `scripts/rejected-proposals.txt` 中 `well-known` / `agent-card` / `Agent Card` 三条的理由当前是「已实现」。该前提已被阶段 0 推翻（实现为私有协议），改为指向本 ADR，否则 C13 会继续以错误理由拦截。
  - ADR-0017 状态行「实现了标准 A2A 1.0 最小子集」与代码不符，按描述性订正，并追记指向本 ADR。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 提示词选项 (a)：独立 `polaris mesh` 进程，作为本机 stdio MCP Server，内部用 A2A 客户端调用对端 | 多一个进程、多一跳。Polaris 之间本可以直接 MCP 对 MCP，mesh 只是一个协议翻译层。它唯一不可替代的价值（调用外部 A2A Agent）保留为可选的 `a2a-bridge` |
| 提示词选项 (b)：修订 ADR-0084，在内核内置 A2A 出站 | Tool Layer / Task Layer 第二套交叉实现的理由（ADR-0084 反例守护第一条）没有因为 SDK 稳定而消失；而且出站需求已经被 MCP 门面满足，没有新事实推动推翻 |
| OpenClaw 式 Hub + Node（一个大脑，其他机器做无状态手脚） | Polaris 每台机器上已经是完整实例（桌面版 + VPS 两种形态并存，ADR-0096），本期需求是实例间委派而非远程执行。Node 能力（远程 exec / 文件 / 本地模型）可以作为 `/mcp` 门面后续追加的工具类别，与本 ADR 正交，届时另立 ADR |
| NATS / Redis 等消息中间件 | 违反单二进制与 State-in-DB；黑板本身就是带崩溃恢复的队列 |
| 跨节点同步数据库 | 违反记忆隔离与 HE-7 |
| 入站继续自研 A2A 协议栈 | 官方 SDK 体积增量 <1MB、无 CGO，自维护 proto 对齐的成本远高于引入依赖 |

## 引用代码

- `internal/gateway/server/sysadmin/a2a/admin_a2a.go`（现入站，将被替换）
- `internal/gateway/server/server_routes.go:43-44`、`internal/gateway/server/middleware_auth.go:56,173,222,278`
- `internal/execute/orchestrator/mcp_a2a_worker.go:127,155-157`、`internal/execute/orchestrator/default_worker.go`、`cmd/polaris/boot_agent.go:641-642`
- `internal/extension/mcp/mcp_client_era.go:18`、`mcp_client_tasks.go`、`mcp_client.go:97`
- `internal/security/network/safe_dialer.go:30`、`internal/security/token/capability_token.go`
- `internal/agent/pool.go:325`（headless 一律 TaintHigh）、`internal/store/repo/repo_task.go:42`
- `docs/arch/M07` / `M08` / `M11` / `M13` 描述性章节（落码时同步）

## 重新评估触发条件

1. 需要 Polaris 调用非 Polaris 的 A2A Agent 且频率足以抵消一个子进程的成本 → 启用 `a2a-bridge`；如果届时 ADR-0084 的 MCP 出站链路被证明无法承载 A2A 流式语义，再重议是否在内核内置出站。
2. 用户的主要场景转为「一个大脑 + 远程手脚」（例如 VPS 调度、Mac 只提供文件 / 浏览器 / 本地模型）→ 另立 ADR 引入 Node 能力工具类别。
3. MCP 或 A2A 规范发布不兼容的新主版本，或 claude.ai 连接器认证方式变化。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-12 | 初稿（Proposed，待用户审阅） |
| 2026-10-12 | 用户审阅通过，转 Accepted。Tailscale 不做专用集成（不内嵌 tsnet、不调用 tailscale CLI）：它对 Polaris 只是一个三层网络，决策六的「显式监听地址 + 精确 host:port 放行」对 Tailscale 地址和局域网地址同样成立；内嵌 tsnet 会给 Tier-0 二进制增加约 10MB 依赖并引入第二套身份体系，收益只是省去安装 Tailscale 客户端。文档给出 Tailscale 组网的推荐配置即可 |
| 2026-10-12 | 实现落地（第一、二期），与正文不同之处如下，正文保持原样以便追溯：①对端令牌不用 `internal/security/token` 的 Ed25519 能力令牌，改为 256 位随机不透明令牌（`pmt_` 前缀）+ 库内只存 SHA-256（`mesh_peer_tokens`）——那套 TokenManager 的签名密钥是进程内临时生成的，重启即失效，且签发方 = 校验方时签名不比查表多提供任何安全性，查表还天然支持列表与吊销；②「写操作转 HITL」改为对端任务以只读角色执行（`AgentProfileSpec.ReadOnly`，写文件/起进程/改状态的工具在执行入口硬拒绝，`[mesh] peer_read_only` 可关），并默认禁止再委派（`allow_relay=false`，从源头杜绝成环），比转 HITL 更严且不会让对端任务卡在本机无人值守的审批上；③对端登记存数据库 `mesh_peers` 而非 `[[mesh.peers]]` 配置：配对需要程序化写入固定公钥，改写用户手写 TOML 不可靠；配对流程为 `polaris peer invite` / `polaris peer accept` 两次复制粘贴（人即确认者），对方回邀请时才把预签发令牌绑定到真实节点 ID；④出站令牌存于对应 MCP Server 的 `Authorization` 头（与其他 MCP Bearer 凭证同一存放方式），未单独进 Vault；⑤`get_remote_task` 不另做内置工具——对端 `/mcp` 暴露的 `get_task` 工具即是，Polaris 客户端轮询 `tasks/get` 失联超过 120s 时，错误信息里带对端任务 ID 指向它；⑥`capabilities.extendedAgentCard` 本期为 false；⑦旧端点 `POST /v1/a2a/tasks` 默认关闭（`legacy_endpoint`），同时修复其任务 ID 取自调用方、`SpawnDepth` 恒 0 两个缺陷；⑧`PostTask` 补写 `intent_taint` 列。**未完成**：OTel 跨节点 span / Prometheus 指标 / `audit_log`（阶段 5）、`a2a-bridge`、流式接口；**已知偏差**：`ARCHITECTURE.md §1` 要求远程绑定带 TLS，本期 `extra_listen` 为明文 HTTP，依赖 Tailscale（WireGuard）提供链路加密，绑定到非 Tailscale 地址时启动告警——重新评估触发条件增补：出现不经 Tailscale 的跨机部署需求时，用节点身份密钥签发自签证书 + 对端按固定公钥校验，补 TLS 监听 |
| 2026-10-12 | 本机双实例实测（两个独立守护进程 + 真实 CLI + 真实 LLM）后修订，单测未覆盖的缺陷一并修复：①EgressGateway 域名白名单层同样只放行 `mesh_peers` 登记的精确 host:port（此前 SafeDialer 放行了、上一层仍拦）；②对端 MCP Server 以 trust_tier 2 登记（`peer accept` 本身即人的确认，默认 auto_review 免二次审批，结果仍 TaintHigh）；③重新配对一轮完成（对方邀请写明发给本节点且本节点仍持有有效令牌即视为已配对），新邀请作废未接受的旧邀请令牌，对端首次使用新令牌时作废其更早的令牌；④ADR-0084 出站链路两处既有缺陷：Worker 代调 `a2a_delegate` 等契约工具时没有能力令牌，trust_tier≤2 的目标一律被执行闸门拒绝（`MCPManager.SetDelegationTokenMinter` 只为契约工具补发一次性令牌）；S_EXECUTE→S_AWAIT_AGENT 与恢复两条转移零效应，watcher 从不启动，`transfer_to_agent` 之后父任务永不恢复（cd93d66 回归）；⑤对端角色收紧：只读角色看不到写类工具，禁止委派角色看不到 MCP 委派通道（`mcp__*__a2a_delegate[_async]` 等），越权计划在 S_VALIDATE 前段打回重规划而不是落到污点闸门转本机 HITL；MCP 工具无能力等级元数据，只读角色一律视为可写（fail-closed）；⑥**只读不等于不泄露**：进程级文件白名单含整个 DataDir，只读的 `read_file` / `data_query` 可读到 `vault.key` 与 `polaris.db`。新增 `AgentProfileSpec.FileRoot`，内置文件/命令工具只能访问该目录（替换而非追加白名单，根非法时 fail-closed），对端任务固定为 `[mesh] peer_file_root`（默认 `<data_dir>/workspace/peer-shared`）；⑦`DefaultTaskWorker` 启动时补扫待认领任务——认领只由 `task_posted` 事件驱动，重启前投递未认领的任务此前要饿到 reaper 的 60 分钟超时；⑧`/v1/mcp-servers` 的 mcpadmin 子处理器 MCPManager 从未回填（建档后不立即连接、`/test` 恒 503）。**范围外待办**：MCP 工具能力等级缺省为 CapReadOnly 导致 S_VALIDATE 污点闸门对 MCP 工具放行，属全局问题，另立 ADR |
