# 模块 7: Tool & Action Layer

> MCP 双向化 | 三级沙箱 | 能力分级 read_only→privileged | Go+Rust 沙箱 | [HE-Rule-2] [HE-Rule-5]
> CANONICAL SOURCE: 沙箱架构、Rust 脚本沙箱、StreamingActionBus
> **§跳读**: 0-bis:6 职责 / 0-ter:18 不变量速查 / 1:29 MCP双向 / 2:83 A2A / 3:113 注册 / 4:165 三级沙箱(CANONICAL) / 5:300 PolicyGate / 6:355 Capability / 7:372 动作扩展 / 8:507 Usage演化 / 12:548 (SOFT)降级 / 13:566 跨模块契约 / 14:586 Plugin / 15:628 Hook
## 0-bis. 职责边界

- M7 **是**: 工具注册中心（ToolRegistry）+ 五大工具类别管理 | M7 **不是**: 工具的语义定义者（各模块注册自己的工具）
- M7 **是**: MCP 双向桥接（Host 消费 + Server 暴露） | M7 **不是**: MCP 协议实现（依赖 Go MCP SDK）
- M7 **是**: 沙箱分级执行（Sbx-L1/L2/L3） | M7 **不是**: 沙箱选型决策（那是 M11 PolicyGate）
- M7 **是**: Capability Token 签发与校验（沙箱门口） | M7 **不是**: 凭证长期存储（那是 M11 CredentialVault）
- M7 **是**: ExecuteTool 调用入口（含 SideEffectPreCheck） | M7 **不是**: 工具调用时机决策（那是 M4 状态机）
- M7 **是**: 技能脚本执行（Logic Collapse Python 脚本委托 ContainerSandbox；普通 script 技能委托 InProcessSandbox） | M7 **不是**: 技能发现与匹配（那是 M6 SkillSelector）
- M7 **是**: 所有出站网络连接强制经 M11 SafeDialer | M7 **不是**: 网络策略制定（那是 M11 SSRFGuard）

---

## 0-ter. 不变量速查表

- 编号: inv_M7_01 | 不变量: 所有工具（含 builtin）必须经 Capability Token 验证——禁止后门路径 | 验证方式: CI `no_backdoor_lint`
- 编号: inv_M7_02 | 不变量: MCP 获取内容默认 taint=high——trusted_sources 白名单例外 | 验证方式: M11 Connector-Taint-Table
- 编号: inv_M7_03 | 不变量: 沙箱选择不可被调用方手动覆盖——`AssignSandboxTier()` 由 M11 PolicyGate 决定 | 验证方式: 代码审计
- 编号: inv_M7_04 | 不变量: Capability Token 委托链最大深度 3——权限只能收缩不可放大 | 验证方式: M7 §4.6 ValidateDelegation
- 编号: inv_M7_05 | 不变量: 不可逆操作（write_network/privileged）执行前须 DryRun + HITL | 验证方式: M7 §5.3 Shadow Sink + §5.4 DryRunMode
- 编号: inv_M7_06 | 不变量: 所有出站连接强制经 M11 SafeDialer.DialContext——禁止裸 net.Dial/grpc.Dial/http.Get | 验证方式: CI `safe_dialer_lint`

---

## 1. MCP 双向架构

MCP Server 暴露端: `mcp.Server("polaris-agent" v0.1.0)` → 注册工具 `execute_skill`（skillLib.LookupByName→Wasm执行） + 注册资源 `memory://episodic/recent`（memory.Episodic().GetRecent(20)）

MCP Client 消费端: `ConnectExternalMCP(serverCmd)` → CommandTransport/StreamableHTTP → OAuth 2.1 + PKCE 认证（远程 server）→ ListTools → 发现外部工具

**MCP OAuth 2.1 + PKCE 流程**: Client → 重定向至 IdP → 带回 access_token → 注入 MCP request header `Authorization: Bearer <access_token>`。OAuth access_token 用于跨进程认证；Capability Token 用于本地沙箱内的能力收缩，两者分工明确。

**Streamable HTTP** 为默认远程传输层；SSE 仅向后兼容（legacy）。决策见 [ADR-0017](./decisions/ADR-0017-mcp-streamable-http-default.md)。

**MCP Transport 污点保护反序列化**：MCP Client 路径强制使用 TaintPreservingDecoder（`internal/action/` + `internal/extension/mcp/`），禁用标准 JSON 直解动态 schema——所有 string 叶子包装为 TaintedString（Source=MCP, Origin=server_name），初始 `[TaintLevel]` 按 M11 §2.4 `[Connector-Taint-Table]` 判定。决策与被驳回方案见 [ADR-0018](./decisions/ADR-0018-mcp-taint-preserving-decoder.md)。

**MCPManager.CallTool 直接路径安全**：`MCPManager.CallTool` 提供面向外部调用方的直接路由接口。该入口在调用 MCP Client 前强制执行 `PolicyGate.IsAuthorized`（deny-by-default），信任等级根据服务器是否在白名单（Trusted）动态设置。与 `InMemoryToolRegistry.ExecuteTool` 保持一致的安全语义，两条路径均不绕过策略层。

**MCP stdio 子进程环境隔离（R1.15）**：stdio 子进程启动时禁止直接继承父进程完整环境，必须经 `internal/extension/mcp/` 的父环境净化逻辑过滤 `*_KEY/_TOKEN/_SECRET/_PASSWORD` 等密钥类键名，再叠加显式配置。Go exec 在 Env 为 nil 时同样继承完整父进程环境，因此必须显式赋值，不得依赖条件分支。

**MCP 工具注册路径与 AssignSandboxTier 的关系**：`AssignSandboxTier` 规定 `ToolMCP → SandboxWasm`，描述的是**工具调用层**的沙箱要求。`MCPManager` 的实际执行路径是：MCP 工具以 `InProcessRichFn` 注册到 `InProcessSandbox`，执行时通过 JSON-RPC 调用外部 MCP 进程（stdio/HTTP），外部进程本身即为隔离边界。Wasm 级别的隔离发生在 MCP Server 进程内部，而非宿主侧 `InProcessSandbox.Run`。这是当前已知的沙箱层级差异，Tier 0 macOS 无 microVM 时接受此设计，Tier 1+ Linux 可在 MCP Server 进程外再套 Firecracker。

**LoadFromDB 必须读取 trust_tier**：启动时从 `mcp_servers` 表加载 MCP Server 时，必须 SELECT `trust_tier` 并以 `trust_tier >= 3` 设置 `MCPClientConfig.Trusted = true`，否则所有服务器（含官方）均降级为 `TaintHigh`，破坏 publisherTrustMap 的设计意图。

统一错误映射（transport-agnostic）:

- 传输层: stdio | 典型错误: broken pipe, EOF, exit≠0 | Code: CONNECTION_LOST
- 传输层: stdio | 典型错误: stderr JSON-RPC error | Code: REMOTE_ERROR
- 传输层: SSE (legacy) | 典型错误: 连接超时>30s | Code: CONNECTION_TIMEOUT
- 传输层: Streamable HTTP | 典型错误: 连接超时>30s | Code: CONNECTION_TIMEOUT
- 传输层: Streamable HTTP | 典型错误: 无 close frame | Code: CONNECTION_LOST
- 传输层: HTTP | 典型错误: 502/503/504 | Code: REMOTE_UNAVAILABLE
- 传输层: HTTP | 典型错误: 4xx / 401 OAuth 过期 | Code: CLIENT_ERROR
- 传输层: HTTP | 典型错误: DNS/TCP 失败 | Code: CONNECTION_FAILED
- 传输层: gRPC (A2A) | 典型错误: UNAVAILABLE/DEADLINE_EXCEEDED | Code: REMOTE_UNAVAILABLE
- 传输层: gRPC (A2A) | 典型错误: PERMISSION_DENIED | Code: CLIENT_ERROR

重试: CONNECTION_LOST/FAILED/TIMEOUT→2次指数退避；CLIENT_ERROR→0；REMOTE_ERROR/UNAVAILABLE→1次

### 1.1 MCP 扩展原语与安全护栏 (Sampling/Roots/Elicitation/Prompts)

M7 桥接层对 MCP 扩展原语强制物理级护栏：

- **Sampling (Server 请求本地 LLM)**
  - Deny-by-default，仅 `allow_sampling` Capability Token 放行
  - Empty Context 隔离执行，禁止携带 ImmutableCore / Episodic
  - 产出强制 `[TaintHigh]`（禁入 instruction slot），Token 计入 Session 预算，受 M3 TokenBurnRate 熔断
- **Roots (Server 请求文件树边界)**
  - 严格收敛于 Sandbox `allowed_paths`，拦截真实路径，仅返回沙箱挂载点（如 `/tmp/sandbox/{skill_id}/`），防目录穿越与探活
- **Elicitation (Server 请求人工交互)**
  - 禁止阻塞宿主，封装为异步结构化意图 → M8 `[Blackboard]` → M13 HITLGateway 展示
  - 默认 5 分钟超时未响应 → 向 Server 返回异常，防会话死锁
- **Prompts (Server 提供预置模板)**
  - 视为不可信外部负载，强制 `ZoneTaintedData`
  - 初始 `[TaintLevel]`=High（白名单 Server → Medium），配合 Spotlighting 防间接 Prompt Injection

---

## 2. A2A Agent Card

```json
{
  "name": "Polaris Code Reviewer", "version": "1.0.0",
  "url": "http://localhost:9090/.well-known/agent-card.json",
  "capabilities": {"streaming": true, "pushNotifications": false},
  "authentication": {"schemes": ["bearer"]},
  "defaultInputModes": ["text", "file"], "defaultOutputModes": ["text", "file"],
  "skills": [{"id": "go-concurrency-review", "name": "Go Concurrency Review",
    "description": "Detects goroutine leaks, race conditions, and channel misuse",
    "tags": ["go", "concurrency", "code-review"]}]
}
```

Agent Card 服务端路径: `/.well-known/agent-card.json`（A2A v0.3+ 强制）。远程 Agent Card 签名校验见 M11 §2.6 VerifyExternalAgentCard。

**ContainerSandbox Linux 命名空间隔离**（`internal/sandbox/sandbox_linux.go`）：
当 SandboxRouter 路由至 `ContainerSandbox`（Tier1+ Linux）时，`cmd.SysProcAttr` 自动注入：
- `CLONE_NEWPID`：子进程获得独立 PID 命名空间，无法枚举/信号攻击宿主进程
- `CLONE_NEWNS`：独立挂载命名空间，防止子进程污染全局 mount 表
- `CLONE_NEWUSER`：独立用户命名空间，子进程映射为非特权 uid
- `Pdeathsig=SIGKILL`：父进程退出时自动 SIGKILL 子进程，消灭孤儿
非 Linux 平台（`sandbox_other.go`）返回 nil，路由层已降级至 WasmSandbox，不到达此路径。
`ContainerSandboxSysProcAttr()` 已导出，供 `bash` 工具和 Hook Runner 复用相同的隔离属性。
**[计划中]** Landlock LSM 文件系统白名单（`LandlockRestrictSelf` + `POLARIS_SANDBOX_EXEC` reexec 模式）尚未在 `sandbox_linux.go` 中实现；当前 ContainerSandbox 仅注入 PID/Mount 命名空间隔离。
A2A 同进程黑板模式（M8）；跨机: HTTP/gRPC 端点。构建时按部署配置选择。

---

## 3. 工具注册系统

Tool/CapabilityLevel/SideEffect/RiskLevel/SandboxTier/ToolSource/ToolResult 类型定义见 `internal/protocol/types.go`。ToolRegistry 接口见 `internal/protocol/interfaces.go`。
其中 `ToolResult` 支持携带 `ImageParts []ImagePart`，解决 MCP 等外部工具返回图片数据的能力需求。

Schema 版本化（防技能断裂）: 新增可选字段=Patch, 新增必填字段=Minor, 移除/重命名字段=Major。Minor/Patch 向后兼容；Major→Logic Collapse 重生成（`needs_adaptation`）。工具来源: Built-in(~20) | MCP(inf) | Skill(inf) | A2A(inf) | LLM-generated(临时，[Sandbox-L3])

**ToolRegistry.Quarantine(toolID, reason)**: 工具发现安全漏洞时紧急摘除——调用后该工具立即从可用列表摘除，所有进行中调用接收 ErrToolQuarantined。配套 CLI `polaris tool quarantine <toolID>`。

**OAuth Scope 字段**: Tool 类型补充 OAuthScope 字段，MCP 远程工具与 Cedar 策略关联:
  - `mcp:{server_id}:{scope}` token 格式
  - Cedar 策略可基于 scope 做 permit/forbid 决策

### 3.1 核心内置工具层 (Built-in Tools)

Polaris L1 层提供生存套件（Survival Kit），以 Go 原生代码直接执行，提供最高性能且受限于原生沙箱策略。

核心内置工具清单（`internal/tool/builtin/` 共 22 个 + `git_text_tools.go` 的 git 工具），严禁在外部或扩展层绕过它们：

- **文件读写**：`read_file`、`write_file`、`list_dir`、`str_replace_editor`（防漂移编辑器，含 undo 缓冲）、`multi_edit`（单文件多处原子修改）、`glob`（受限目录/文件模式匹配）
- **搜索**：`grep`（工作区全文正则）
- **网络**：`fetch_url`（SafeDialer 出站保护）、`web_search`（原生网页检索）
- **执行**：`bash`（平台原生沙箱）、`run_command`（受限构建，如 `go test`）、`execute_wasm`（Wasm 沙箱执行）、`data_query`（工作区结构化数据查询）
- **辅助**：`todo_read`/`todo_write`（任务列表持久化）、`notebook_read`/`notebook_edit`（Jupyter Notebook）、`read_tool_ref`（工具定义自省）
- **Git**：`git_diff`、`git_commit`（`git_text_tools.go`，Agent 可直接调用无需 MCP 扩展）
- **记忆（LLM 主动写）**：`memory_write`（写入语义事实）、`memory_search`（混合检索记忆）、`memory_append`（追加属性到已有实体）、`memory_expire`（标记事实失效）— 详见 M05 §5-bis。通过 `builtin.RegisterMemoryTools(sbx, toolReg, semanticWriter, retriever)` 统一注册（`internal/tool/builtin/memory_tools.go`）。

> 历史遗留的 Wasm 版 `file_read`/`file_write`/`web_fetch`/`shell_exec` 技能已全部废弃并清理。

**write_file 禁止目录黑名单**：`makeWriteFileFn`（`internal/tool/builtin/`）在 `checkAllowedPath` 通过后追加 `checkForbiddenPath` 二次过滤，硬编码禁止写入 `~/.polarisagi/polaris/config/`、`~/.polarisagi/polaris/data/`、`~/.ssh/`、`~/.gnupg/`、`/etc/`、`/usr/`、`/bin/`、`/sbin/` 等敏感目录；当 `os.UserHomeDir()` 获取失败时兜底禁写 `/root/.polarisagi`；路径比较走 `filepath.Clean + strings.HasPrefix`，不可被配置覆盖。

**Git 内置工具**：`internal/tool/builtin/git_text_tools.go` 已将 `git_diff` 和 `git_commit` 实现为 M07 原生内置工具（`makeGitDiffFn` / `makeGitCommitFn`），Agent 可直接调用，无需挂载外部 MCP 扩展。Git 高级操作（rebase、branch 管理等）仍可通过 L3 MCP 扩展扩充。

### 3.2 平台原生进程沙箱（WrapBashCmd）

`internal/tool/sandbox/native_sandbox.go` — 为内置 `bash` 和 `run_command` 工具提供进程级隔离，与 Wasmtime Wasm 沙箱互补（Wasmtime 管 Wasm 技能，原生沙箱管系统进程）。

`WrapBashCmd(ctx, NativeSandboxCfg) (*exec.Cmd, error)` 按平台分发：

| 平台 | 实现 | 隔离机制 |
|------|------|---------|
| macOS | Apple Seatbelt (`sandbox-exec`) | **已退化（跳过）**：因 `deny-default` 导致 Deno/Node 瞬间崩溃（abort trap），现依赖运行时（如 Deno `--allow-*`）自身防线 |
| Linux | bubblewrap (`bwrap --unshare-pid/uts/ipc`) | rootfs ro-bind 系统目录 + bind 工作目录 + namespace 隔离（**保留网络能力，不强制 `--unshare-net`**） |
| Windows | WSL2 (`wsl.exe --cd`) | VM 级隔离；网络管控提示需 Windows 防火墙 |
| 降级 | `wrapFallback` | env 清理 + 无额外 namespace（仅最后防线） |

配置入口：`configs/defaults.toml [sandbox]`（`enabled=true`, `network_policy=block`）→ `internal/config.SandboxConfig`。
`RegisterBuiltinTools(sandboxEnabled, netPolicy, bwrapPath)` 将配置注入各工具闭包。
bwrap 不可用时自动降级 `wrapFallback`（WARN 审计），不向上层抛出错误。

---

## 4. 三级沙箱架构（CANONICAL SOURCE）

### 4.1 Tier x Platform 能力矩阵

L3 平台选择由 `AutoConfig.computeSandboxConfig()` 自动化，`FeatureGate.FeatureL3Sandbox` 检测平台+内存。Tier 0 内存不足 → ErrFeatureUnavailable，不降级原生子进程。调用方仅检查 `GlobalFeatureGate().IsEnabled(FeatureL3Sandbox)`。详见 M03 §5。

L3 平台原生 microVM (统一 SandboxProvider 接口，调用方平台无感):
- **Linux**: Firecracker (~125MB/VM, 需硬件 KVM)；KVM 不可用 → gVisor (runsc) 用户态内核
- **macOS**: Virtualization.framework (~80MB/VM)
- **Windows**: WSL2 + Hyper-V (~150MB/VM)

Tier 0 L3 不可用: 全平台 Tier 0 内存不足启动 microVM (每 L3 ≥256MB)。CapWriteNetwork/Privileged 在 Tier 0 → ErrTier0SandboxLimit。**不提供原生子进程降级**（避免突破安全底线）。

### 4.2 自动分级

`AssignSandboxTier(tool, hwTier, goos) -> SandboxTier`（实现见 `internal/sandbox/assign.go`）:
1. Source→最小级别: Builtin→InProcess；LLMGenerated→Wasm；MCP/A2A→Wasm
2. Capability提升: WriteNetwork+→Wasm；Privileged→Container
3. SideProcessSpawn→Container
4. Tier0 Container 请求全平台拒绝：`AssignSandboxTier` 返回 `(SandboxTier, error)`；hwTier==0 需要 Container 的工具返回 `ErrTier0SandboxLimit`，不区分 goos，不降级 Wasm，调用方必须显式处理（M07 §4.2）。

**脚本风险评估默认沙箱等级**：来源不明或 LLM 生成的脚本默认分配 L2 Wasm 隔离等级；仅明确白名单来源（builtin）的脚本可走 L1 InProcess。

当前 `sandbox.go` 实现与规则 1–4 对齐：`LLMGenerated`/`MCP`/`A2A` 分配 L2 Wasm，`CapWriteNetwork` 最低 L2，`CapPrivileged`/`SideProcessSpawn` 升至 L3；Tier0 Container 全平台返回 `ErrTier0SandboxLimit`，不降级。

Auto-Curriculum: M9 `bash_restricted` 强制 L2 Wasm，字符集 `[A-Za-z0-9 ./\-_=:,]`，禁止管道/重定向/命令替换/`~/.polarisagi/polaris`。`bash` 永久禁止。

### 4.3 Rust 脚本沙箱（CANONICAL SOURCE）

Wasm 沙箱由 Rust Wasmtime 引擎驱动，通过 purego FFI 桥接至 Go 层。实现文件：`internal/tool/sandbox/wasmtime_sandbox.go`（Go 封装）+ `internal/tool/sandbox/rust_wasmtime_sandbox.go`（purego FFI 桥）。

`WasmtimeSandbox.Run` 执行流程：DryRun 模式直接返回 mocked 结果 → `CalculateWasmQuota(systemTier, taintLevel)` 计算内存页数（maxPages）与 fuel 配额 → `WasmtimeExecute(scriptBytes, inputJSON, workspaceDir, memoryPages, networkAllowed, fuel, maxOutputBytes)` 调用 Rust 引擎完成编译、实例化与执行 → 返回 `ToolResult{Success, Output, LatencyMs}`。

启动时 `WasmtimeInit()` 初始化引擎，`WasmtimePoolInit(5)` 预热 5 个实例。编译缓存与生命周期管理由 Rust Wasmtime 引擎内部处理，Go 层不自管缓存。

**[计划中]**：PII SecureUnredact / PostExecution Redact、Host Functions FS/网络权限矩阵挂钩（`WASIPermission`/`ResourceLimits` 结构体已定义，未接入执行路径）。

资源硬限制（超限→ErrSandboxResourceExhausted，不重试）：

| 维度 | Built-in | User | LLM生成 |
|------|---------|------|--------|
| CPU / 壁钟 | 30s / 90s | 10s / 30s | 5s / 15s |
| 调用次数 | 10000 | 5000 | 2000 |
| I/O 总量 | 100MB | 10MB | 1MB |
| 内存(maxPages) | 256 (16MB) | 128 (8MB) | 64 (4MB) |

**SandboxSpec tier 一致性**: `SandboxRouter.Execute` 传入 `SandboxSpec.SandboxTier` 为 `AssignSandboxTier` 升级后的实际 tier，确保审计日志与执行层一致。

共用约束: 每次 I/O ≤ 64KB，同函数 ≤ 100 calls/s（超频 throttle 10ms），Host Func 单次 ≤ 100ms（**仅限低层 I/O 原语**；MCP/A2A 宿主侧独立运行不受此限）；超额 → cancel 优雅 → 1s 后 CloseWithExitCode 强制。
强制契约: 阻塞调用前必须 `select{case <-ctx.Done():return ctx.Err() default:}`，CI lint `host_func_audit` 强制检查。
Syscall 防逃逸: Go 堆缓冲区（严禁线性内存切片）→ 独立 goroutine 执行 → ctx.Done() 检查 → copy() 回线性内存；ctx 取消 → 丢弃 + recover()。
长程任务（>4min）须异步: Wasm 提交参数 → M13 TaskQueue 执行 → 返回 JobID → 轮询进度；CapToken 续期绑定宿主侧 Worker，Wasm 90s 壁钟硬限不变（compile=10×5min/crawl=8×5min/index=8×5min）。

### 4.4 WASI 权限矩阵

- WASI 能力: fd_read/write | L1 Built-in: stdio only | L2 User: stdio only | L3 LLM生成: stdio only
- WASI 能力: path_open (read) | L1 Built-in: 工作目录+/usr/local/bin/ | L2 User: /tmp/sandbox/ | L3 LLM生成: /tmp/sandbox/{skill_id}/
- WASI 能力: path_open (write) | L1 Built-in: 工作目录 | L2 User: /tmp/sandbox/ | L3 LLM生成: /tmp/sandbox/{skill_id}/
- WASI 能力: sock_send/recv | L1 Built-in: Egress白名单域名 | L2 User: ❌ | L3 LLM生成: ❌
- WASI 能力: clock_time_get | L1 Built-in: ✅ | L2 User: ✅ | L3 LLM生成: ✅
- WASI 能力: random_get | L1 Built-in: ✅ | L2 User: ✅ | L3 LLM生成: ✅
- WASI 能力: environ_get | L1 Built-in: ❌ | L2 User: ❌ | L3 LLM生成: ❌
- WASI 能力: proc_exit | L1 Built-in: ❌ | L2 User: ❌ | L3 LLM生成: ❌
- WASI 能力: args_get | L1 Built-in: 仅tool input JSON | L2 User: 仅tool input JSON | L3 LLM生成: 仅tool input JSON

目录挂载: Builtin→/workspace+/usr/local/bin(只读)；User→/tmp/sandbox；LLMGenerated→/tmp/sandbox/{skill_id}(0700)

### 4.5 Workspace Bridge

`workspace_read(artifactID,offset,length)->([]byte,error)`:
- 0 路径校验: filepath.Clean→分量级`..`检测+IsAbs拦截。Linux 5.6+: `Openat2(workspaceRootFd, path, RESOLVE_NO_SYMLINKS|RESOLVE_IN_ROOT)`。非Linux: component-by-component walk→逐级openat+Fstat校验dev/inode
- 0.1 读取禁止路径（eval 数据隔离）: 目标路径前缀匹配以下任一 → 立即返回 ErrEvalDataAccessForbidden + CRITICAL 审计，不触发 Capability Token 校验（快速拒绝，防止绕过）:
    `~/.polarisagi/polaris/eval/holdout/`（Holdout Set，防 M9 过拟合，M12 §5）
    `~/.polarisagi/polaris/eval/training/`（Training Set，仅 Eval API 层允许 M9 通过 Ed25519 签名访问，不走 Workspace Bridge）
  注: `~` 展开为运行时 polaris_home 变量（与 M11 Cedar Layer 2 的 context.polarisagi/polaris_eval_holdout_path 同源）。物理层 Openat2(RESOLVE_IN_ROOT) 已阻止路径逃逸，此检查为防御纵深。
- 1 验证Capability Token读权限→2 `Pread(fd,buf,offset)`→3 每次<=64KB→4 Audit Trail

`workspace_write(artifactID,data)->(int,error)`:
- 0 路径白名单校验: 仅允许写入以下三类路径:
  (a) `~/.polarisagi/polaris/workspace/<task_id>/`（M2 WorkspaceManager 托管目录）
  (b) 经 [Sandbox-L2] 显式挂载的临时目录 `/tmp/sandbox/{skill_id}/`
  (c) 启动时传入的 Workspace Root（用户项目根目录），需经 [Cedar-Gate] 显式授权——Cedar 策略 `permit write_local when resource.path in WorkspaceRoot` 控制可写子路径范围
  默认拒绝所有其他绝对和相对路径。白名单外路径 → ErrWorkspacePathNotAllowed + WARN + 审计
- 0.1 禁止覆盖保护: 即使白名单内，仍禁止覆盖 `~/.polarisagi/polaris/config/`、`~/.polarisagi/polaris/secrets/`、`~/.polarisagi/polaris/data/`（含 SQLite/SurrealDB-Core 数据库文件）、`~/.polarisagi/polaris/audit/`——此四目录为系统关键数据区，独立于 Workspace 白名单做第二层拒绝
- 前置: CapabilityLevel>=write_local
- 0.5 Taint Gate（路径 × TaintLevel 决策表）:

  | 路径 | TaintLevel | 结果 |
  |------|-----------|------|
  | (c) Workspace Root | ≤Medium | 允许；TaintLevel 继承+[Taint-Prop]；Cedar 做最终授权 |
  | (c) Workspace Root | ≥High | ErrTaintGateBlockedWrite；重定向 ephemeral/ |
  | (a) task workspaces | ≤Medium | 允许；TaintLevel 继承+[Taint-Prop] |
  | (a) task workspaces | ≥High | ErrTaintGateBlockedWrite；需人工介入 |
  | (b) 临时沙箱 | ≤Low | 允许；TaintLevel 继承 |
  | (b) 临时沙箱 | ≥Medium | ErrTaintGateBlockedWrite；重定向 ephemeral/ |
- 每次<=64KB，追加模式

### 4.6 Capability 委托链

委托链两层实现：
- `policy.TokenManager.Delegate(parent, agentID, caps, ttl)` — 协议层：能力取交集、TTL/2 衰减、`DelegatedFrom` 携带父 TokenID；
- `action.ValidateDelegation(parentToken, depth, agentID, ...)` — 业务层：深度≥3 → ErrMaxDelegationDepth，effectiveCaps = intersect；
- `policy.TokenManager.ValidateDelegation(parent, child)` — 验证 `child.DelegatedFrom==parent.TokenID`、caps ⊆ parent.caps、ExpiresAt ≤ parent.ExpiresAt、SandboxTier 不降级。
规则摘要：
- 规则1 权限收缩: child.Caps ⊆ parent.Caps；交集为空 → ErrForbidden
- 规则2 沙箱单调: child.SandboxTier >= parent.SandboxTier（L2→L1拒绝）
- 规则3 溯源: DelegatedFrom 链。业务层 DerivationDepth>=3 → 拒绝
- 规则4 TTL 衰减: child.ExpiresAt ≤ parent.ExpiresAt，Delegate 默认 TTL = 父剩余 / 2
- 规则5 MCP隔离: MCP Client 子进程在调用方沙箱上下文启动（继承 WASI 权限+Capability 约束）

运行时策略重检: Host Function I/O前比对Cedar policy etag与Wasm实例化时policy_etag_at_start。etag变更→重调[Cedar-Gate] Review→FORBID返回ErrPolicyRevoked。etag比对O(1)，仅变更时触发完整评估。

TOCTOU防御: (a)read_local等可逆→接受窗口；(b)write_network等不可逆→PreCheck→I/O→PostCheck 三重校验:
  PreCheck: etag + M8 Blackboard Lease Version 比对
  I/O: 执行工具调用（须尊重 ctx.Done()——M8 Reaper 回收任务前会 cancel agent context）
  PostCheck: 重新校验 etag + Lease Version。etag 变更→审计 policy_etag_diverged_during_io(CRITICAL)+标记 policy_stale；Lease Version 不匹配（任务已被 Reaper 回收）→ 审计 side_effect_orphaned(CRITICAL) + 写入 M3 decision_log（非 events 表——任务所有权已转移至新 Agent，孤儿记录仅作审计轨迹，不参与 Blackboard 状态变更）
(c)privileged→不走快捷路径，每次完整 PolicyGate.Review。M8 Reaper 在重置任务前先 `cancel(agent.ContextCancel)` 并等待 5s 宽限期（M8 §1.7），工具实现须在 5s 内通过 ctx.Done() 感知取消并中止

配额隔离: per-instance（10000/5000/2000），非全局共享。深度3+白名单+per-instance=三层纵深。

### 4.7 L3 策略监视器

L3 microVM (Firecracker / Virtualization.framework / WSL2) 的 I/O 路径不经过 Wasmtime host function——syscall 由 VM 内核栈或 gVisor sentry 拦截。这些路径均不触发 Host Function 层的 etag 重检，因此需要独立的策略监视器。

L3PolicyMonitor goroutine (每个 L3 sandbox 一个):
- (a) 订阅 [Cedar-Gate] SubscribeEtagChange（channel 推送）
- (b) etag 变更 → 重评 CapToken: 允许 → 更新 etag；FORBID → 三级阻断:
    (1) 关闭网络出口（各平台拆除方式见 `internal/sandbox/sandbox_impl.go`）
    (2) context cancel M4（通知 Agent 任务被策略吊销）
    (3) 审计 `l3_policy_revoked_network_killed` (CRITICAL)
- (c) 毫秒级阻断，闭合 etag 变更到下一次 I/O 之间的策略逃逸窗口（最长 4min CapToken TTL 窗口）
- (d) L3 不可用时（Tier 0 全平台 / 平台 microVM 不可用）→ L3PolicyMonitor 不实例化

---

## 5. Policy Gate

### 5.1 Cedar DSL

```cedar
forbid(principal, action, resource) when {
    resource.source == "llm_generated" && !resource.has_been_reviewed };

permit(principal in Role::"Agent", action == Action::"call_tool", resource) when {
    resource.capability == "read_only" && context.session_trust_level >= 1 };

permit(principal in Role::"Agent", action == Action::"call_tool", resource) when {
    resource.capability == "write_local" && context.session_trust_level >= 2 &&
    resource.allowed_paths.contains(context.target_path) };

permit(principal in Role::"Agent", action == Action::"call_tool", resource) when {
    resource.capability == "write_network" && context.session_trust_level >= 3 &&
    context.approval_status == "approved" };
```

### 5.2 5阶段执行

- **Gate1 Cedar 策略评估**: FORBID → 触发 HITL
- **Gate2 Capability Token 验证**: Ed25519 签名 + 5min TTL + scope + 未撤销
- **Gate3 Rate Limiter**: 全局 / 每工具 / 每 Session
- **Gate4 Taint 追踪**: 外部输入标记 tainted，不入 system prompt。工具参数逐字段标 TaintLevel——任一 ≥ `[Taint-Medium]` 且 Capability ≥ write_network → 拒绝（需 SanitizeByUserReview）。覆盖文件写入（§4.5）+ 网络出口（M11 SafeDialer）两层
- **Gate5 出站预检**: 目标 URL 静态 CIDR 匹配（阻 `127.0.0.0/8` `10.0.0.0/8` `172.16.0.0/12` `192.168.0.0/16` `::1`），白名单域名放行。实际连接委托 M11 SafeDialer（§6）执行五阶段 SSRF 防护（DNS + TOCTOU + IP 锁定）
- 通过 → 执行 L1/L2/L3

三层防线: 语义([Cedar-Gate])→数据([TaintLevel])→网络(SSRF)

**trust_level 动态推导**: `InMemoryToolRegistry.ExecuteTool` 向 PolicyGate 传入的 `trust_level` 刷根据工具来源（`tool.Source`）动态计算：Builtin → 4（系统信任），MCP/A2A → 2（社区信任），其余 → 1。`capability_token_valid` 根据 `tool.Capability <= CapReadOnly` 动态设置。Cedar 策略中 `trust_level >= N` 的条件正确生效。

### 5.3 Shadow Sink

write_network/privileged 不可逆操作:
- Phase1 Shadow Dry-Run: 路由到 `localhost:{port}/_shadow/{tool_name}`→基于output_schema自动生成mock响应→Agent Kernel验证schema一致性
- Phase2 HITL: 展示工具名+参数摘要+Shadow结果→approve生成一次性Token(TTL 见 `spec/state.yaml §m7_tool.dryrun_protect_window_seconds`)/deny拒绝
- Phase3 Real Execution: 一次性Token签发→执行

`generateMockFromSchema`: string→"[MOCK:{name}]"；number→min/max中值；boolean→false；array→1元素；object→递归

### 5.4 DryRunMode

ToolMeta Reversible=false 时，M7.DryRun(call): (a)参数校验(schema+类型+范围)→(b)权限检查→(c)目标存在性(本地stat/域名白名单+SSRF CIDR/recipient格式)——**禁止真实网络请求**(TCP dial/HTTP HEAD/DNS)→(d)副作用预估(bytes/rows/cost)→返回DryRunResult{Feasible,Warnings[],EstimatedImpact,Reason}。feasible+无warning→自动执行；feasible+有warning→HITL；not feasible→error

- Tool类型: builtin(str_replace_editor) | DryRun行为: 参数校验+目标字符串存在唯一性+权限+撤销缓冲预估
- Tool类型: builtin(run_command) | DryRun行为: 命令解析+白名单+参数验证
- Tool类型: MCP proxy | DryRun行为: 仅本地Schema校验+SSRF预检，禁止转发真实请求
- Tool类型: skill(wasm) | DryRun行为: 调用validate()入口，否则schema-only

DryRun结果→[EventLog]（tool.dry_run_result），Reflexion回顾。

---

## 6. Capability Token

短寿命权限令牌，沙箱门口 JIT 签发。实现见 `internal/action/capability_token.go`。

**JIT Minting 流程**:
Planner 决定调用工具 → 生成 ToolIntent（不签发 Token）→ M8 Blackboard CAS 认领 → HITL 审批（可能 10+ 分钟）→ Worker 进入沙箱 Gate → Gate1-5 全过 → JIT Mint Token (MaxCalls=1, TTL=5min) → 沙箱执行。
审批期间无有效 Token 泄露。

**Token 续期**: 长任务（预估 >4min）宿主 goroutine 每 (TTL-60s) 续期。续期校验:
  - 租约: Agent Lease 未过期
  - 策略: 比对 Cedar-Gate etag；变更则 PolicyGate.Review 重评——允许 → 新 Token + 更新 etag；FORBID → 取消沙箱 + 审计 `token_renewal_policy_revoked` (CRITICAL)
默认 MaxRenewals=5 次（30min 窗口）。长程覆盖: compile=10 次/60min, crawl=8 次/48min, index=8 次/48min。

**委托链溯源**: 每 Token 记录 ParentID，最大深度 3。effectiveCapability = min(caller, target)——权限只缩不放。

---

## 7. 动作空间扩展

### 7.1 Computer Use / GUI 自动化架构

**架构定位**: 采用独立的外部 MCP 插件/微服务（如通过插件市场分发的 Computer MCP Server）作为底层驱动。`computer_use` 能力不再作为硬编码的系统内置侧车运行，而是通过标准的 MCP 协议扩展接入，这确保了主干 Agent 进程的绝对安全与生命周期解耦。严禁在外部扩展中内嵌任何核心 AI 模型（OmniParser / VLM 均保留在主干推理网关）。

**核心技术栈**:
1. **感知层 (Sensor)**:
   - **截图**: 使用 `xcap` ([GitHub](https://github.com/nashaofu/xcap)) 跨平台截屏。
   - **语义 UI 树**: 直接调用 OS 原生 API (Win: `uiautomation-rs`, Mac: `axuielement`, Linux: `AT-SPI2`)，弃用抽象封装库以确保精度。
2. **执行层 (Actuator)**:
   - **键鼠注入**: 使用 `enigo` ([GitHub](https://github.com/enigo-rs/enigo)) 跨平台模拟。
   - **Linux 特化兜底**: 弃用实验性的 Wayland `libei`，直接采用 `evdev` 向 `/dev/uinput` 写入内核级输入信号。

Execute: ForegroundIntent→physical；BackgroundTask/AutoCurriculum→headless。
- **physical 模式**: 依赖外部的 Computer MCP 插件服务。
- **headless 模式**: Tier 1+ → `Xvfb :99 -screen 0 1280x800x24` 启动虚拟显示器执行。

**执行耗时追踪**: 底层追踪表（如 `decision_log` 或 `agent_actions`）必须录入 `created_at` 与 `updated_at` 时间戳，供前端渲染耗时。

GUI Action Loop: see→decide→act循环maxSteps次。Capture+UITree→VLM DecideAction(在主干)→发送MCP Command→executeAction(left_click/type等)→GUIResult

**HITL 拦截门控**（`interceptComputerUse`，`internal/agent/agent_execute.go`）:
- 触发工具: `computer_use` 和 `browser_use`（均需经此门控）
- 受全局 `permission_mode` 控制（由 `PreferencesRepo.GetPermissionMode` 注入）:
  - `full_access`: 自动放行（依赖 Cedar 的最终防线预检）
  - `auto_review`（默认）: 仅危险动作触发 HITL
  - `default`: 所有调用均触发 HITL
- 危险动作定义:
  - `computer_use`: `key / type / left_click / right_click / double_click / left_click_drag`
  - `browser_use`: `click / type / key`
- HITL 路径: `a.hitl.Prompt()` → 用户 approve/deny → deny 返回 `CodeForbidden`（任务中止）

### 7.2 LAM（Large Action Model）— ComputerUseEngine

实现见 `internal/action/lam/lam.go`（`ComputerUseEngine`，实现 `LargeActionModel` 接口）。将自然语言意图转为 GUI 动作并执行，路径：`intent + ScreenState → VLM → computerUseArgs JSON → ExecutorFn`。

**硬件门控**: `FeatureComputerUseGUI`（`internal/observability`）在 `ExecuteAction` 入口前检查。未通过 → 返回 `ToolResult{Success:false, Error:"FeatureComputerUseGUI not enabled"}`，不抛错。

**双路径架构**:
```
Tier 0 / DOM-only 路径（screenshot == nil 或 >2MB）:
  userContent = "DOM 结构:\n{dom}\n\n用户意图: {intent}"
  → 零图片 token，保护 Tier 0 内存预算

Tier 1+ / vision 路径（screenshot ≤ 2MB）:
  userContent = "屏幕分辨率: {W}x{H}\nDOM 结构:\n{dom}\n\n用户意图: {intent}"
  → base64(ScreenshotBytes) 注入 protocol.Message.Parts
```

**VLM 响应结构** (`vlmActionOutput`):
```
action:     screenshot | left_click | right_click | mouse_move | type | key
coordinate: [x, y]（可选）
text:       输入文本（可选）
reasoning:  推理说明（仅日志，不转发 executor）
```

**ExecutorFn 注入模式**: `executor ExecutorFn` 由调用方注入（通常 `action.NewComputerUseTool().Execute`），解耦 `internal/action/lam` 与 `internal/action` 父包，防止循环依赖。`executor=nil` → dry-run 模式，返回解析的动作 JSON 供调试。当前 boot 以 `sb.Router` 作为 VLM provider 注入，executor 暂为 nil（dry-run），待 Tier-1+ GUI 执行器接入后填充。

**Agent Kernel 集成**（ADR-0027 BUG-1）: `ComputerUseEngine` 提供导出方法 `CheckPolicy(ctx context.Context, actionJSON []byte) error`，由 `Agent.interceptComputerUse`（`internal/agent/agent_execute.go`）在 HITL 审批**前**调用，实施 Cedar `browser_automate/lam/{allow_net:true}` deny-by-default 预检。Agent struct 持有 `lamEngine *lam.ComputerUseEngine` 字段，boot 通过 `agent.SetLAMEngine(lamEngine)` 注入。`lamEngine==nil` 时跳过 Cedar 预检（nil-safe，兼容无 LAM 场景）。调用顺序：Cedar PolicyGate（快速拒绝）→ HITL 审批（人工确认）。

**LAMConfig**:
```
Enabled:        bool
PerceptionMode: string  // "auto" (按内存自动降级) | "local_omniparser" (强制本地) | "cloud_vlm" (强制云端多模态)
ResolverModel:  string  // 视觉解析模型，如 "deepseek-chat" 或 "claude-3-5-sonnet"
```

**ActionDiscretizer**（`internal/action/lam/continuous_action.go`）: 已实现基于余弦相似度的离散化投影（`Discretize` 方法，含 `keyToCentroid` + `cosineSim` 算法）。连续向量 → 离散工具调用，延迟 ~1-5ms。Vision 解析路径（DisplayServer）待 Tier-1+ 接入。

### 7.3 StreamingActionBus（CANONICAL SOURCE）

`internal/action/lam/streaming_action_bus.go` 已实现 `StreamAction()`（含速率控制令牌桶 + ActionClipper 向量钳制 + maxSteps=1000 步数限制）。`DisplayServer` 接口已定义，平台适配（Xvfb/VNC/Wayland）待 Tier-1+ 接入，nil 时以 no-op 安全降级。

StreamAction 已实现流程：步数检查（超 maxSteps 返回错误）→ 令牌桶速率控制（阻塞直到获得令牌或 ctx 取消）→ ActionClipper 向量钳制（可选）→ DisplayServer.SendAction（nil 时 no-op 安全降级）。

**实现状态**：令牌桶（滑动窗口+令牌桶混合）和 ActionClipper 已在 `internal/action/lam/streaming_action_bus.go` 完整实现；类型校验和 ASCII 白名单属于文档设计目标，当前 `StreamAction` 对 `ContinuousAction.ActionType` 不做枚举约束，由调用方保证合法性。

M4 S_EXECUTE路由: tool_call→ActionDiscretizer→ToolCall；mouse_delta→StreamingActionBus→虚拟帧缓冲；key_sequence→StreamingActionBus→虚拟帧缓冲；其他→ErrUnsupportedActionType

LAM路径: 工具调用→ActionDiscretizer(~1-5ms离散化)；GUI→StreamingActionBus(<0.5ms背压)；混合→按Type分流并行双通道

Security: StreamingActionBus不绕过Capability——mouse_delta/key_sequence需CapGUIAutomation（Tier1+默认启用，Tier0需显式授权）。

### 7.4 `[CodeAct]` — 即时代码执行行动空间

> 对齐 2024 CodeAct (Wang et al.) / OpenInterpreter / SWE-agent。区别于 [Logic-Collapse]（沉淀型 Python 技能，走 staging 流水线后写入 Skill Library）——CodeAct 是 **ad-hoc 一次性代码 + 立即执行**，不沉淀。共用 ContainerSandbox 执行路径，运行时依赖一致。

**用途**: M4 S_EXECUTE 可选行为空间。当任务需要"组合多个工具 + 中间计算 + 条件分支"时，LLM 直接生成 Python/JS 代码片段，由 M7 执行——比多次 tool_call 更紧凑、组合性更强。

**`inv_global_07` 强制约束**（无豁免）:
- Source: `ToolSource=LLMGenerated`
- Sandbox: 强制 Level≥3（ContainerSandbox），`policyGate` + `sandbox.Level()` 双重门控，任一不满足立即 fail-closed
- Capability Token: 由调用方通过 `req.CapabilityID` 传入（CodeAct 不自行 Mint，调用方须事先 JIT 签发）
- Cedar 策略: deny-by-default，需 `permit code_act when ... approval_status == "approved"`；`policyGate` 未注入时 fail-closed
- Audit: 完整代码 + stdout + exit_code，通过 `toolExec.RecordAudit(ctx, "code_act", auditPayload)` 写入 EventLog（已实现，`internal/action/codeact/code_act.go`）

**`Execute` 已实现并接线**（`internal/action/codeact/code_act.go`）：语言校验（python|bash）→ CapabilityID 非空检查 → Cedar 策略评估（fail-closed）→ sandbox.Level()≥3 断言 → 构造 SandboxSpec 调用 sandbox.Run → 返回 CodeActResult。`boot_server.go` 在 `FeatureL3Sandbox` 启用时构造引擎，注入 `agent.SetCodeAct()` 和 `httpServer.SetCodeActEngine()`（Batch 3+4 完成）。

Taint 检查、16KB 代码大小限制、PostExec PII Redact 属于文档设计目标，**[计划中]**。

**与 Logic Collapse Python 技能的区别**:

| 维度 | CodeAct (本节) | Logic Collapse Python 技能 (M6) |
|------|---------------|--------------------------------|
| 沉淀 | 否（一次性） | 是（写入 Skill Library，HE-6） |
| Staging 流水线 | 不走 | 完整 7 阶段（ValidatePython + Eval Gate + 签名）|
| Sandbox | L3 ContainerSandbox（共用） | L3 ContainerSandbox（共用） |
| 执行路径 | `writeTempScript` → sandbox.Run | SkillRegistry 读 skill.py → sandbox.Run |
| 用途 | 即时复杂组合 | 高频可复用模式（零 LLM System 1）|
| 触发 | M4 显式选择 | M9 自动后台触发（≥50 成功轨迹）|
| HT0 可用 | 否（L3 依赖）| 否（L3 + FeatureLogicCollapse 双门控）|

**M4 S_EXECUTE 决策树**（扩展 §5 RouteReasoning）:
- 单工具调用 → 标准 tool_call
- 组合 ≥3 工具 + 中间计算 → 候选 CodeAct（M4 询问 LLM 偏好 + Cedar 校验）
- 高频可复用 → 后台进入 Logic Collapse / Auto-Curriculum 沉淀

### 7.5 GovernanceAgent 代码安全三层防线

LLM 生成代码（CodeAct / LLMGenerated Wasm）在进入沙箱前须经 `GovernanceAgent.ValidateCodeWithAudit` 依序过三层：

| 层 | 性质 | 机制 | 失败结果 |
|----|------|------|---------|
| Layer 0 | 同步，<5ms | Go AST 解析 + import 路径白名单扫描；拦截危险包导入（`os/exec`/`syscall`/`unsafe` 等） | 硬拒绝，不进入 L1 |
| Layer 1 | 同步，<1ms | 正则规则集（`code_validator.go` TS006 等），邻近匹配距离 ≤200 字节防跨行误报 | 硬拒绝，不进入 L2 |
| Layer 2 | 异步，独立 goroutine | 单次 LLM 调用（`ThinkingMax`），`SecurityAuditAgent` 出具结构化安全报告 | 返回审计结论，高风险则拒绝执行 |

Layer 0/1 为同步卡口，失败直接阻断代码进入沙箱。Layer 2 为深度异步审计，仅在 L0/L1 通过后并发启动，不阻断热路径但审计结论必须在沙箱执行前到达（超时则 fail-closed）。

实现见 `internal/swarm/agents/governance_agent.go`、`internal/swarm/agents/code_validator.go`、`internal/swarm/agents/security_audit_agent.go`。

---

## 8. Tool Usage Policy Evolution

Logic Collapse (M6) 创建新技能，本机制提升已有工具使用策略——从历史调用轨迹学习最优参数和失败模式。

### 8.1 与 Logic Collapse 的分工

- 维度: 进化对象 | Logic Collapse (M6): 创建新技能 | Tool Usage Policy Evolution (M7): 已有工具的使用策略
- 维度: 触发条件 | Logic Collapse (M6): 50+ 成功 | Tool Usage Policy Evolution (M7): 每次调用后更新统计
- 维度: 输出 | Logic Collapse (M6): Python 技能（src/skill.py）+ SKILL.md | Tool Usage Policy Evolution (M7): 策略提示词 + 参数建议
- 维度: 粒度 | Logic Collapse (M6): 工具级 | Tool Usage Policy Evolution (M7): 调用级

### 8.2 策略模型

持久化类型定义见 `internal/action/tool_usage_policy.go`:

- **ToolUsagePolicy** — 工具的最优参数建议和适用场景。字段: `ToolName` / `ParamHints map[string]ParamHint`（最优参数建议）/ `BestFor []string`（适用场景）/ `NotRecommendedFor []string`（不适用场景）
- **ParamHint** — 参数级别的最优值约束。字段: `DefaultValue any` / `Description string` / `MinValue any` / `MaxValue any`

以下数据由 PolicyEvolver（§8.3）运行时维护，不持久化:
- **FailurePattern** — 失败模式签名（ErrorType × 输入特征），含频率计数和 LLM 生成的缓解策略
- **CoToolPattern** — 工具组合模式（ToolName × Relationship ∈ {before, after}），按频率排序
- **运行时统计** — `SuccessRate`（加权平均）、`AvgLatencyMs`、`UseCount`，每次调用后更新

### 8.3 PolicyEvolver

`internal/action/tool_usage_policy.go` 实现：

- `RecordOutcome`：SuccessRate 滑动窗口加权更新 + 失败模式提取（ErrorType+频率，连续 3 次同类失败自动生成缓解建议）
- `GetContextHint(toolName)`：历史调用 ≥20 次时返回 ParamHint 建议 + 高频失败警告；否则返回空（冷启动不注入噪声）
- `BuildSystemHintBlock()`：聚合所有已激活工具的提示，生成标准 `<tool-hints>...</tool-hints>` XML 块，供 M4 DAG 构建 InferRequest 时注入 System Prompt 的 ZoneMutableSkill 区。无任何提示时返回空字符串（调用方不注入）。

**注入位置**：M4 DAG 节点执行前，调用 `PolicyEvolver.BuildSystemHintBlock()` 获取聚合提示块，注入 System Prompt ZoneMutableSkill 区。不修改工具定义和 schema。

**启用条件**：单工具 ≥20 次历史调用自动激活。Tier 0 低频下以冷启动默认值运行；Tier 1+ 持续优化。

---

> 安全闭环: [TaintLevel] [Taint-Prop]→[Cedar-Gate]→CapabilityToken→[Sandbox-L1/L2/L3]→RateLimit→[EventLog]

---

## 12. 降级与失败模式

- 故障场景: 沙箱启动失败 (L2 Wasm) | 降级路径: 拒绝执行该工具 + [EventLog] | 恢复策略: 重启沙箱宿主后恢复
- 故障场景: 沙箱启动失败 (L3 gVisor) | 降级路径: 拒绝执行该工具 + AuditEvent | 恢复策略: gVisor 重装后恢复
- 故障场景: Capability Token 校验失败 | 降级路径: 拒绝执行 + AuditEvent | 恢复策略: 重新申请 Token
- 故障场景: MCP 外部 server 不可达 | 降级路径: mark_unreachable → 该工具从可用列表移除 | 恢复策略: 心跳恢复后重新注册
- 故障场景: Rate Limiter 触发 | 降级路径: 429 排队 + Retry-After | 恢复策略: 窗口过期自动恢复
- 故障场景: 不可逆操作 DryRun 失败 | 降级路径: 拒绝执行 + HITL 升级 | 恢复策略: 手动审批后签发一次性 Token
- 故障场景: Linux Firecracker KVM 不可用 | 降级路径: 降级 gVisor (runsc) 用户态内核 | 恢复策略: KVM 可用后自动切换回 Firecracker
- 故障场景: macOS Virtualization.framework 不可用 (旧版本系统) | 降级路径: L3 不可实例化 → ErrFeatureUnavailable | 恢复策略: 系统升级后恢复
- 故障场景: Windows WSL2 不可用 | 降级路径: L3 不可实例化 → ErrFeatureUnavailable | 恢复策略: 启用 WSL2 + Hyper-V 后恢复

与 OSMemoryGuard 协同: L1 预警 → 禁止启动新 Wasm 实例 / L2 紧急 → kill 空闲沙箱 / L3 临界 → kill 全部非关键沙箱，仅保留当前交互任务。

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m7_tool`。

## 13. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces.go` + `types.go`。本表仅列依赖方向 + 一句话语义 + 锚点。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M4→M7 | ToolRegistry.ExecuteTool | DAG S_EXECUTE 节点调用；tool_call→Registry→Policy Gate→Sandbox→Execute→ToolResult。M4 §4 |
| M6→M7 | Sandbox L2 Wasm Runner | Wasm 二进制→`[Wasm-Sandbox]` Wasmtime（Rust FFI purego）+ WASI 权限矩阵。M6 §5 |
| M1→M7 | Tool Schema | LLM tool_call 传入 InputSchema。M1 §2 |
| M8→M7 | SideEffectPreCheck | 每次 ExecuteTool 前强制执行。M8 §1.6 |
| M9→M7 | bash_restricted L2 | 强制 Wasm + Ephemeral Namespace。M9 §2.2 |
| M7→M2 | EventLog session_events | Tool Call audit trail；Workspace Bridge → WorkspaceManager VFS 代理。M2 §2.1, §3 |
| M7→M11 | Cedar-Gate / CredentialVault / SafeDialer | 策略评估 / Token 验证 / 出站统一出口。M11 §3, §5, §6 |
| M7→M11 | TaintTracker | TaintLevel 传播。M11 §2 |
| Schema | Tool / ToolResult / CapabilityLevel / ToolRegistry | `internal/protocol/types.go`, `interfaces.go` |
| 全局字典 | Sandbox-L1/L2/L3、Wasm-Sandbox、Cedar-Gate、CredentialVault | 00-Global-Dictionary §4, §5 |
| 时序图 | Taint Tracking 全链路（外部输入→SanitizeBySchema→workspace 写入）| DIAGRAMS.md#taint-tracking |

---

## 14. Plugin Registry（ADR-0015 §2.1）

> End-User 可通过 Plugin Bundle（tar.gz）打包分发技能+MCP 组合，无需修改源码。
> 参见 [ADR-0015](./decisions/ADR-0015-codex-feature-integration.md) 与 M13-bis §3.3。

**Plugin manifest 格式** (`plugin.json`，即 `PluginBundleManifest`）:
```json
{
  "name": "github",
  "version": "1.0.0",
  "description": "GitHub MCP integration",
  "mcp_inline": {
    "github-mcp": { "command": "npx", "args": ["-y", "@github/mcp-server"] }
  },
  "mcp_servers": ".mcp.json",
  "skills": [{ "path": "skills/pr-review/SKILL.md", "name": "pr-review" }]
}
```

同一 bundle 目录下还可同时包含外部厂商格式（`ai-plugin.json` / `plugin.toml` / `skills.yaml`），由 `adapter.ParseManifestDir()` 解析后各自安装对应的运行时组件。

**安装路径**：`~/.polarisagi/polaris/extensions/plugin/{ext_id}/`（HTTP tar.gz 下载解压）

**加载流程**:
```
POST /v1/plugins/install → plugin_catalog.go.downloadAndInstallPlugin()
  → 解析 plugin.json（PluginBundleManifest）
  → mcp_inline / .mcp.json  → installBundleMCP() → mcp_servers + MCPManager.Add()
  → skills[]                → installBundleSkill() → skills（runtime=script）
  → adapter.ParseManifestDir() 处理外部厂商格式
  → INSERT plugins（021）写 bundle 元数据
```

**安全约束**:
- 所有子路径通过 `safeJoin()` 校验，防止 bundle 内路径穿越到安装目录外
- Plugin Bundle MCP 默认 Taint=High（M7 inv_M7_02）
- Script Skills trust_tier 继承 extension_catalog

**代码位置**: `internal/gateway/server/plugin/catalog.go`（安装）、`internal/extension/marketplace/adapter.go`（多厂商解析）、`internal/extension/marketplace/loader.go`（Polaris 原生格式）

---

## 15. Hook 框架（ADR-0015 §2.2）

> End-User 可在生命周期事件注入 shell 脚本（零依赖），对应 ARCHITECTURE.md §1 `[ShellHooks]` 设计意图。
> 输出强制 TaintLevel=High，通过 M11 PolicyGate 才可注入 Agent 上下文。

**事件触发点**:

| 事件 | 触发模块 | 说明 |
|------|---------|------|
| `SessionStart` | M13 Gateway | 连接建立后 |
| `PreToolUse` | M7 (sandbox 执行前) | 支持工具名 matcher 正则 |
| `PostToolUse` | M7 (sandbox 执行后) | 携带工具输出（stdout） |
| `UserPromptSubmit` | M13 消息入队 | 携带原始 prompt |
| `Stop` | M4 FSM S_IDLE | Agent 回到空闲 |

**配置格式** (`~/.polarisagi/polaris/hooks/hooks.yaml`):
```yaml
hooks:
  PreToolUse:
    - matcher: "^Bash$"
      hooks:
        - type: command
          command: "/path/to/pre_tool_check.sh"
          status_message: "Checking command"
          timeout: 30s
  Stop:
    - hooks:
        - type: command
          command: "/path/to/session_summary.sh"
```

**安全不变量**：
- Hook 脚本输出封装为 `TaintLevel=High` 的 TaintedString，不得直接注入 Immutable Zone
- Hook 执行超时 30s（可配置），超时不中断主流程；超时告警由调用方负责处理
- 并发 Hook（同事件多个匹配）由 errgroup 并发执行，互不影响
- **环境变量隔离**: Hook 子进程仅继承最小化 PATH，不继承宿主进程完整环境
- **Linux namespace 隔离**: 自动注入 `ContainerSandboxSysProcAttr()`（PID + 挂载 namespace），与 ContainerSandbox.RunScript 保持一致的隔离策略

**代码位置**: `internal/action/hook/` (hook.go / runner.go / registry.go / hook_linux.go / hook_other.go)

> **✅ 已修复（native_sandbox.rs mutex 中毒）**：`rust/substrate/src/native_sandbox.rs` 中 stdout/stderr 采集子线程的 `buf.lock().unwrap()` 已改为 `unwrap_or_else(|e| e.into_inner())`，锁中毒时取回内部数据而非 panic，子线程不再因锁异常丢失输出。
