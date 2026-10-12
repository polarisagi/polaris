# 模块 7: Tool & Action Layer

> MCP（Model Context Protocol，模型上下文协议） 双向化 | 三级沙箱 | 能力分级 read_only→privileged | Go+Rust 沙箱 | [HE-Rule-2] [HE-Rule-5]
> CANONICAL SOURCE: 沙箱架构、Rust 脚本沙箱、StreamingActionBus
<!-- §跳读: 0-bis:6 职责 / 0-ter:20 不变量速查 / 1:33 MCP双向 / 2:66 A2A（Agent-to-Agent，智能体间通信） / 3:74 注册 / 4:130 三级沙箱(CANONICAL) / 5:282 PolicyGate / 6:343 Capability / 7:355 动作扩展 / 8:434 616 Usage演化 / 12:468 658 (SOFT)降级 / 13:485 676 跨模块契约 / 14:505 696 Plugin / 15:521 738 Hook -->
## 0-bis. 职责边界

| M7 是 | M7 不是 |
|---|---|
| 工具注册中心（ToolRegistry）+ 五大工具类别管理 | 工具语义定义者（各模块注册自己的工具） |
| MCP 双向桥接（Host 消费 + Server 暴露） | MCP 协议实现（依赖 Go MCP SDK） |
| 沙箱分级执行（Sbx-L1/L2/L3） | 沙箱选型决策（M11 PolicyGate） |
| Capability Token 签发与校验（沙箱门口） | 凭证长期存储（M11 CredentialVault） |
| ExecuteTool 调用入口（含 SideEffectPreCheck） | 工具调用时机决策（M4 状态机） |
| 技能脚本执行（Logic Collapse Python → ContainerSandbox；普通 script 技能 → InProcessSandbox） | 技能发现与匹配（M6 SkillSelector） |
| 所有出站网络连接强制经 M11 SafeDialer | 网络策略制定（M11 SSRFGuard） |

---

## 0-ter. 不变量速查表

- 编号: inv_M7_01 | 不变量: 所有工具（含 builtin）必须经 Capability Token 验证——禁止后门路径 | 验证方式: CI（Continuous Integration，持续集成） `no_backdoor_lint`
- 编号: inv_M7_02 | 不变量: MCP 获取内容默认 taint=high——trusted_sources 白名单例外 | 验证方式: M11 Connector-Taint-Table
- 编号: inv_M7_03 | 不变量: 沙箱选择不可被调用方手动覆盖——`AssignSandboxTier()` 由 M11 PolicyGate 决定 | 验证方式: 代码审计
- 编号: inv_M7_04 | 不变量: Agent 任务派生深度最大 3 | 验证方式: M8 `orchestrator.MaxSpawnDepth`（M7 §4.6-bis；Token 委托链已移除）
- 编号: inv_M7_05 | 不变量: 不可逆操作（write_network/privileged）执行前须 DryRun + HITL（Human-in-the-loop，人机协同） | 验证方式: M7 §5.3 Shadow Sink + §5.4 DryRunMode
- 编号: inv_M7_06 | 不变量: 所有出站连接强制经 M11 SafeDialer.DialContext——禁止裸 net.Dial/grpc.Dial/http.Get | 验证方式: CI `safe_dialer_lint`

> 进入此模块前必读 **Tool 层参数校验 + Schema 防御工程化阵阱**：`docs/specs/09-LLM-Agent-Production.md`（HE-Rule-2 实例: A-03/A-06 + P-3）

---

## 1. MCP 双向架构

- **Server 暴露端**：Polaris 间 `/mcp` 门面 + A2A 1.0 `/a2a/v1` 门面，见 [ADR-0111](./decisions/ADR-0111-cross-node-delegation-mcp-facade-and-a2a-1-0.md)（`internal/gateway/server/meshapi/`）。
- **Client 消费端**：`internal/extension/mcp`（`MCPManager`）；传输 stdio / Streamable HTTP（默认）/ SSE（legacy）；远程 server 走 OAuth 2.1 + PKCE，access_token 注入 `Authorization: Bearer`（跨进程认证；Capability Token 负责本地沙箱内能力收缩，二者分工）。决策见 [ADR-0103](./decisions/ADR-0103-extension-dual-vendor-standard.md)（原 ADR-0017/0018）。
- **污点保护反序列化**：Client 路径强制 TaintPreservingDecoder（`internal/extension/mcp/taint_decoder.go`），禁用标准 JSON 直解动态 schema——所有 string 叶子包装为 TaintedString（Source=MCP, Origin=server_name），初始 `[TaintLevel]` 按 M11 §2.4 `[Connector-Taint-Table]` 判定。
- **`MCPManager.CallTool` 直接路径**：调用 Client 前强制 `PolicyGate.IsAuthorized`（deny-by-default），信任等级按服务器是否在白名单（Trusted）设置；与 `InMemoryToolRegistry.ExecuteTool` 同一安全语义，两条路径均不绕过策略层。
- **stdio 子进程环境隔离（R1.15）**：禁止继承父进程完整环境，必须经 `internal/extension/mcp/` 的环境净化过滤 `*_KEY/_TOKEN/_SECRET/_PASSWORD` 键名，再叠加显式配置；Go exec 在 Env 为 nil 时同样继承父环境，故必须显式赋值。
- **沙箱层级**：`AssignSandboxTier` 对 `ToolMCP` 返回 `SandboxInProcess`（§4.2）；MCP 工具以 `InProcessRichFn` 注册，执行 = JSON-RPC 调外部进程（stdio/HTTP），外部进程即隔离边界。
- **`LoadFromDB` 必须读取 trust_tier**：加载 `mcp_servers` 时 SELECT `trust_tier`，`>= 3` → `MCPClientConfig.Trusted = true`；否则全部降级 `TaintHigh`，破坏 publisherTrustMap。

统一错误映射（transport-agnostic）:

| 传输层 | 典型错误 | Code |
|---|---|---|
| stdio | broken pipe, EOF, exit≠0 | CONNECTION_LOST |
| stdio | stderr JSON-RPC error | REMOTE_ERROR |
| SSE / Streamable HTTP | 连接超时>30s | CONNECTION_TIMEOUT |
| Streamable HTTP | 无 close frame | CONNECTION_LOST |
| HTTP | 502/503/504；gRPC(A2A) UNAVAILABLE/DEADLINE_EXCEEDED | REMOTE_UNAVAILABLE |
| HTTP | 4xx / 401 OAuth 过期；gRPC PERMISSION_DENIED | CLIENT_ERROR |
| HTTP | DNS/TCP 失败 | CONNECTION_FAILED |

重试: CONNECTION_LOST/FAILED/TIMEOUT→2次指数退避；CLIENT_ERROR→0；REMOTE_ERROR/UNAVAILABLE→1次

### 1.1 MCP 扩展原语与安全护栏 (Sampling/Roots/Elicitation/Prompts)

- **Sampling**（Server 请求本地 LLM）：deny-by-default，仅 `allow_sampling` Capability Token 放行；Empty Context 隔离，禁止携带 ImmutableCore / Episodic；产出强制 `[TaintHigh]`（禁入 instruction slot），Token 计入 Session 预算，受 M3 TokenBurnRate 熔断。
- **Roots**（Server 请求文件树边界）：严格收敛于 Sandbox `allowed_paths`，只返回沙箱挂载点（如 `/tmp/sandbox/{skill_id}/`），防目录穿越与探活。
- **Elicitation**（Server 请求人工交互）：禁止阻塞宿主，封装为异步结构化意图 → M8 `[Blackboard]` → M13 HITLGateway；默认 5 分钟未响应 → 向 Server 返回异常，防会话死锁。
- **Prompts**（Server 预置模板）：不可信外部负载，强制 `ZoneTaintedData`；初始 `[TaintLevel]`=High（白名单 Server → Medium），配合 Spotlighting 防间接 Prompt Injection。

---

## 2. A2A 与 ContainerSandbox 进程隔离

- **A2A**：入站 Agent Card（`/.well-known/agent-card.json`，A2A 1.0）与 `/a2a/v1`、出站 `a2a_delegate` 见 ADR-0111；远程 Agent Card 签名校验见 M11 §2.6 VerifyExternalAgentCard。同进程走 Blackboard（M8），跨机走 `/mcp`、`/a2a/v1`。
- **ContainerSandbox**：SandboxRouter 路由至 `ContainerSandbox`（Tier1+）时，`sandbox.CmdRunner`（`internal/tool/sandbox/cmd_runner_adapter.go`）构造 `protocol.SandboxContext` 调用 `native_sandbox_exec_v2`，Rust 层按平台分发：Linux `exec_bwrap_v2`（PID/Mount/User namespace）；macOS `exec_seatbelt_v2`；均不可用 → `exec_bare_v2` / `exec_namespace_fallback_v2`（降级路径）。Go 层不再注入 `SysProcAttr`；`SandboxContext.CallerType`（builtin/hook/codeact/skill/mcp/plugin）决定 `env_preset`；`bash` 工具与 Hook Runner 复用同一 Rust 沙箱调用。
- **文件系统白名单**（非 Landlock，等效实现）：L2 Wasm 靠 WASI capability 路径级白名单（§4.4）；L1/L3 原生进程靠 bwrap / Seatbelt profile。两者均 fail-closed（隔离工具不可用 → 拒绝执行，不静默降级）。Landlock 为可选增强，非缺口。

---

## 3. 工具注册系统

类型定义见 `internal/protocol/types.go`，ToolRegistry 接口见 `internal/protocol/interfaces_*.go`。`ToolResult` 可携带 `ImageParts []ImagePart`（MCP 等外部工具返回图片）。

- **Schema 版本化**（防技能断裂）：新增可选字段=Patch，新增必填字段=Minor，移除/重命名字段=Major。Minor/Patch 向后兼容；Major → Logic Collapse 重生成（`needs_adaptation`）。
- **工具来源**：Built-in(~20) | MCP | Skill | A2A | LLM-generated（临时，[Sandbox-L3]）。
- **`ToolRegistry.Quarantine(toolID, reason)`**（设计条款，代码未实现）：发现安全漏洞时紧急摘除，进行中调用接收 `ErrToolQuarantined`。
- **OAuth Scope**：Tool 带 `OAuthScope`，MCP 远程工具 token 格式 `mcp:{server_id}:{scope}`，Cedar 策略可基于 scope permit/forbid。

### 3.1 核心内置工具层 (Built-in Tools)

L1 层生存套件以 Go 原生代码执行，受原生沙箱策略约束。核心内置工具共 **45 个**、6 个注册入口，均由 `cmd/polaris/boot_tools.go` 装配，严禁在外部或扩展层绕过：

**入口 1 — `builtin.RegisterBuiltinTools`（31 个，元数据来自 `internal/tool/builtin/<name>/tool.yaml` + `schema.json`）**

- 文件：`read_file`、`write_file`、`list_dir`、`str_replace_editor`（防漂移，含 undo 缓冲）、`multi_edit`（单文件多处原子修改）、`glob`；搜索：`grep`
- 网络：`fetch_url`（SafeDialer 出站）、`web_search`
- 执行：`bash`（平台原生沙箱）、`run_command`（受限构建，如 `go test`）、`data_query`
- 数据/文本：`csv_parse`、`diff_text`、`template_render`
- 多媒体：`video_analysis`、`tts`（本地 MeloTTS，经网关 `Synthesize`，ADR-0110）
- 系统：`get_datetime`、`sys_probe`（M03 §5）
- 辅助：`todo_read`/`todo_write`（按会话持久化于 `session_todos` 表，会话 ID 取 `CtxTaskIDKey`，缺失即报错；ADR-0104）、`notebook_read`/`notebook_edit`、`read_tool_ref`
- Git：`git_diff`、`git_commit`（`git_text_tools.go`，原生内置，无需 MCP 扩展；rebase/branch 等高级操作走 L3 MCP 扩展）
- Cron（`cronRepo != nil` 时才注册）：`cron_list`、`cron_create`、`cron_delete`
- Rich 工具（`RegisterRich`，默认 `TaintHigh`）：`execute_wasm`、`get_task_result`（异步任务结果回取）

**入口 2 — `builtin.RegisterMemoryTools`（8 个，`internal/tool/builtin/memory_tools.go`，元数据内联）**：`memory_write`/`memory_search`/`memory_append`/`memory_expire`/`memory_reflect`、`core_memory_edit`（编辑 ZoneCoreMemory 块）、`memory_page_out`/`memory_page_in`。详见 M05 §5-bis 与 ADR-0033。

**入口 3 — `builtin.RegisterSkillTools`（2 个）**：`skill_save`、`skill_generate`（Logic Collapse 蒸馏，M06 §2.2）。

**入口 4 — 惰性目录（`CompositeCatalog`）**：`tool_search`（工具数超过 `m13_interface.lazy_load_tool_threshold` 时按需检索定义，避免全量 schema 撑爆 prompt）。

**入口 5 — `builtin.RegisterDelegationTools`（2 个，`internal/tool/builtin/delegation_tools.go`，ADR-0111 / ADR-0103 决策三）**：`list_agents`（本地子 Agent + `mcp:` A2A 目标 + `general-purpose`）、`transfer_to_agent`（经 S_VALIDATE/PolicyGate；执行由 Agent 内核特判，直接经 Dispatcher 调用返回错误）。装配在 `cmd/polaris/boot_delegation.go`。

**入口 6 — `native.RegisterExtensionTools`（2 个，`internal/extension/native/extension_tools.go`）**：`search_extension`、`install_extension`（InstallManager + HITL）。第三参 `knowledgeSearcher` 当前传 nil；`knowledge_search` 由 `cmd/polaris/boot_knowledge.go` 经 `native.RegisterExtensionTool` 单独注册（M10 §2.5）。

**write_file 禁止目录黑名单**：`makeWriteFileFn`（`internal/tool/builtin/`）在 `checkAllowedPath` 通过后追加 `checkForbiddenPath`，硬编码禁写 `~/.polaris/config/`、`~/.polaris/data/`、`~/.ssh/`、`~/.gnupg/`、`/etc/`、`/usr/`、`/bin/`、`/sbin/` 等；`os.UserHomeDir()` 失败时兜底禁写 `/root/.polaris`；路径比较走 `filepath.Clean + strings.HasPrefix`，不可被配置覆盖。

### 3.2 平台原生进程沙箱（Rust V2 统一沙箱）

`internal/tool/sandbox/rust_native_sandbox.go`（purego FFI 桥）+ `internal/tool/builtin/bash/sandboxed_exec.go`（Go 封装）为 `bash`/`run_command`/`git_*`/`video_analysis`/`tts` 等内置工具提供进程级隔离，与 Wasmtime 互补（Wasmtime 管 Wasm 技能，原生沙箱管系统进程）。V1 接口已删除，全部走 V2 `native_sandbox_exec_v2`。

`runSandboxedArgv` 构造 `protocol.SandboxContext`（`ExecPath`+`ExecArgs` argv 模式，不经 shell，杜绝命令注入）：

| 平台 | 实现 | 隔离机制 |
|------|------|---------|
| macOS | `exec_seatbelt_v2`（`sandbox-exec`） | 按 `CallerType` 选 `env_preset`；`AllowedPaths` 收紧到调用方所需目录，不默认放行 `/` |
| Linux | `exec_bwrap_v2`（bubblewrap） | PID/Mount/User namespace + `AllowedPaths` bind |
| 均不可用 | `exec_bare_v2` / `exec_namespace_fallback_v2` | 降级，env 清理 |

`sandboxEnabled=false` 时退化为 `exec.CommandContext`——仅限有明确技术原因不适合一次性沙箱调用的工具（`audio_convert.go`：合并 stdout/stderr 会破坏二进制 PCM；`display_server_xvfb.go`：Xvfb 为长驻进程），原因见代码注释。

配置：`configs/defaults.toml [sandbox]`（`enabled=true`, `network_policy=block`）→ `RegisterBuiltinTools(sandboxEnabled, netPolicy, bwrapPath)` 注入各工具闭包；bwrap 不可用时降级 `wrapFallback`（WARN 审计），不向上抛错。

---

## 4. 三级沙箱架构（CANONICAL SOURCE）

### 4.1 Tier x Platform 能力矩阵

L3 平台选择由 `AutoConfig.computeSandboxConfig()` 自动化，`FeatureGate.FeatureL3Sandbox` 检测平台+内存；调用方仅检查 `GlobalFeatureGate().IsEnabled(FeatureL3Sandbox)`（M03 §5）。

L3 目标架构（统一 SandboxProvider 接口，平台无感）：Linux Firecracker（~125MB/VM，需 KVM；无 KVM → gVisor runsc）；macOS Virtualization.framework（~80MB/VM）；Windows WSL2 + Hyper-V（~150MB/VM）。**现状**：Firecracker/VZ/WSL2 后端已删除，`ContainerSandbox` 统一经 Rust bwrap/Seatbelt `CmdRunner`（`internal/sandbox/sandbox_container.go`，ADR-0008）。

Tier 0（每 L3 ≥256MB，内存不足）：L3 不可用；Container 请求按 §4.2 降级 NativeOS，`CapPrivileged` 返回 `ErrTier0SandboxLimit`。**不提供无隔离原生子进程降级**。

### 4.2 自动分级

`AssignSandboxTier(tool, trustTier, hwTier, goos) -> (SandboxTier, error)`（`internal/sandbox/assign.go`；error 非 nil 时调用方不得执行工具）:
1. Source：Builtin / MCP → `SandboxInProcess`（MCP 隔离边界为外部进程）；其余来源（LLMGenerated / A2A / Skill）至少 `SandboxWasm`，且不低于 `trustTier.SandboxFloor()`
2. Capability 提升：WriteNetwork+ → 至少 Wasm；Privileged → Container
3. SideProcessSpawn → Container
4. Tier0 + Container：降级 `SandboxNativeOS`（Rust bwrap/Seatbelt，OS 级隔离，非无沙箱原生 exec）；仅 `CapPrivileged` 返回 `ErrTier0SandboxLimit`，调用方必须显式处理（ADR-0008 决策一）

来源不明或 LLM 生成的脚本默认 L2 Wasm；仅 builtin 白名单来源可走 L1 InProcess。`SandboxRouter.Execute` 的 `SandboxSpec.SandboxTier` 为升级后的实际 tier（审计与执行层一致）。

Auto-Curriculum: M9 `bash_restricted` 强制 L2 Wasm，字符集 `[A-Za-z0-9 ./\-_=:,]`，禁止管道/重定向/命令替换/`~/.polaris`。`bash` 永久禁止。

### 4.3 Rust 脚本沙箱（CANONICAL SOURCE）

Wasm 沙箱由 Rust Wasmtime 引擎驱动，经 purego FFI 桥接：`internal/tool/sandbox/wasmtime_sandbox.go`（Go 封装）+ `internal/tool/sandbox/rust_wasmtime_sandbox.go`（FFI 桥）。

`WasmtimeSandbox.Run`：DryRun 直接返回 mocked 结果 → `CalculateWasmQuota(systemTier, taintLevel)` 计算 maxPages 与 fuel → `WasmtimeExecute(...)` 完成编译、实例化、执行 → `ToolResult`。启动时 `WasmtimeInit()` + `WasmtimePoolInit(5)` 预热；编译缓存由 Rust 引擎管理。

- **PII SecureUnredact：已实现**——`internal/agent/context/pii_vault.go` 的 `SessionPIIVault`（AES-256-GCM）提供 `Snapshot`/`Load` 按需还原（`task_pii_vault` 表），M11 §5.1 已同步。
- **Host Functions 权限矩阵**：不用独立 `WASIPermission`/`ResourceLimits` 结构体，分别由 `internal/security/token/capability_token.go`（Ed25519 能力令牌）+ `internal/security/network/safe_dialer.go`（网络出口）+ §4.4 WASI 矩阵（FS）承载。
- **PostExecution Redact**：CodeAct 路径已实现（`internal/action/codeact/code_act.go` 输出端 `RedactWithMode`）；通用 Wasm 路径未见接线。

资源硬限制（超限→ErrSandboxResourceExhausted，不重试；per-instance，非全局共享）：

| 维度 | Built-in | User | LLM生成 |
|------|---------|------|--------|
| CPU / 壁钟 | 30s / 90s | 10s / 30s | 5s / 15s |
| 调用次数 | 10000 | 5000 | 2000 |
| I/O 总量 | 100MB | 10MB | 1MB |

内存上限按沙箱等级 + 资源档位分档（SSoT：`spec/state.yaml §thresholds.m7_tool`，对应 `internal/config/thresholds.go` `M7ToolThresholds`）：`sbx_l2_low_mem_mb`=64（L2 低档）、`sbx_l2_medium_mem_mb`=256（L2 默认，与 `script.max_memory_mb` 一致）、`sbx_l3_high_mem_mb`=512（L3 高档）。

共用约束: 每次 I/O ≤ 64KB，同函数 ≤ 100 calls/s（超频 throttle 10ms），Host Func 单次 ≤ 100ms（**仅限低层 I/O 原语**；MCP/A2A 宿主侧独立运行不受此限）；超额 → cancel 优雅 → 1s 后 CloseWithExitCode 强制。
强制契约: 阻塞调用前必须 `select{case <-ctx.Done():return ctx.Err() default:}`，CI lint `host_func_audit` 强制检查。
Syscall 防逃逸: Go 堆缓冲区（严禁线性内存切片）→ 独立 goroutine 执行 → ctx.Done() 检查 → copy() 回线性内存；ctx 取消 → 丢弃 + recover()。
长程任务（>4min）须异步: Wasm 提交参数 → M13 TaskQueue 执行 → 返回 JobID → 轮询进度；CapToken 续期绑定宿主侧 Worker，Wasm 90s 壁钟硬限不变（compile=10×5min/crawl=8×5min/index=8×5min）。

### 4.4 WASI 权限矩阵

| WASI 能力 | L1 Built-in | L2 User | L3 LLM生成 |
|---|---|---|---|
| fd_read/write | stdio only | stdio only | stdio only |
| path_open (read) | 工作目录+/usr/local/bin/ | /tmp/sandbox/ | /tmp/sandbox/{skill_id}/ |
| path_open (write) | 工作目录 | /tmp/sandbox/ | /tmp/sandbox/{skill_id}/ |
| sock_send/recv | Egress白名单域名 | ❌ | ❌ |
| clock_time_get / random_get | ✅ | ✅ | ✅ |
| environ_get / proc_exit | ❌ | ❌ | ❌ |
| args_get | 仅 tool input JSON | 仅 tool input JSON | 仅 tool input JSON |

目录挂载: Builtin→/workspace+/usr/local/bin(只读)；User→/tmp/sandbox；LLMGenerated→/tmp/sandbox/{skill_id}(0700)

### 4.5 Workspace Bridge

> 设计条款：代码中暂无 `workspace_read`/`workspace_write` 同名实现（2026-10 核实）；`workspace_write` 此前即标注"未实现"。

**`workspace_read(artifactID,offset,length)->([]byte,error)`**:
1. **路径校验**: `filepath.Clean` → 分量级 `..` 检测 + IsAbs 拦截。Linux 5.6+: `Openat2(workspaceRootFd, path, RESOLVE_NO_SYMLINKS|RESOLVE_IN_ROOT)`；非 Linux: 逐级 openat + Fstat 校验 dev/inode
2. **读取禁止路径（eval 数据隔离）**: 前缀匹配以下任一 → 立即返回 `ErrEvalDataAccessForbidden` + CRITICAL 审计，不触发 Capability Token 校验（快速拒绝）:
    - `~/.polaris/eval/holdout/`（Holdout Set，防 M9 过拟合，M12 §5）
    - `~/.polaris/eval/training/`（仅 Eval API 层允许 M9 通过 Ed25519 签名访问，不走 Workspace Bridge）
  > `~` 展开为运行时 `polaris_home`（与 M11 Cedar Layer 2 的 `context.polaris_eval_holdout_path` 同源）；`Openat2(RESOLVE_IN_ROOT)` 已阻止路径逃逸，此检查为防御纵深。
3. 验证 Capability Token 读权限 → `Pread(fd,buf,offset)` → 每次 ≤64KB → Audit Trail

**`workspace_write(artifactID,data)->(int,error)`**:
1. **路径白名单**: 仅 (a) `~/.polaris/workspace/<task_id>/`（M2 WorkspaceManager 托管）、(b) `[Sandbox-L2]` 显式挂载的 `/tmp/sandbox/{skill_id}/`、(c) 启动时传入的 Workspace Root（需 `[Cedar-Gate]` 授权：`permit write_local when resource.path in WorkspaceRoot`）。其他路径 → `ErrWorkspacePathNotAllowed` + WARN + 审计
2. **禁止覆盖**: 即使白名单内，仍禁止覆盖 `~/.polaris/config/`、`secrets/`、`data/`（含 SQLite/SurrealDB-Core 库文件）、`audit/`——独立于白名单的第二层拒绝
3. **前置**: `CapabilityLevel>=write_local`
4. **Taint Gate**（路径 × TaintLevel）:

  | 路径 | TaintLevel | 结果 |
  |------|-----------|------|
  | (c) Workspace Root | ≤Medium | 允许；TaintLevel 继承+`[Taint-Prop]`；Cedar 最终授权 |
  | (c) Workspace Root | ≥High | `ErrTaintGateBlockedWrite`；重定向 `ephemeral/` |
  | (a) task workspaces | ≤Medium | 允许；TaintLevel 继承+`[Taint-Prop]` |
  | (a) task workspaces | ≥High | `ErrTaintGateBlockedWrite`；需人工介入 |
  | (b) 临时沙箱 | ≤Low | 允许；TaintLevel 继承 |
  | (b) 临时沙箱 | ≥Medium | `ErrTaintGateBlockedWrite`；重定向 `ephemeral/` |

5. 每次 ≤64KB，追加模式

### 4.6 Capability Token 委托链（已移除）

`action.ValidateDelegation` / `policy.TokenManager.Delegate` 等委托链机制已删除：`NewJITToken` 唯一生产调用点固定 `depth=0` 单层铸造，委托链从未触发；多 Agent 派生深度由 `MaxSpawnDepth=3`（§4.6-bis）独立覆盖。若未来出现真实的跨 Agent Capability 降权委托场景，应重新设计而非恢复旧实现（`min(caller, target)` 交集语义与深度计数解决的是两个不同问题）。

### 4.6-bis Agent 任务派生深度限制与运行时策略重检

`internal/execute/orchestrator` 的 `SQLiteBlackboard.PostTask`/`PostBatch` 校验 `task.SpawnDepth`，超过 `MaxSpawnDepth=3` 返回 `ErrSpawnDepthExceeded`（纯深度计数，不涉及 Capability 交集）；SQLiteBlackboard 是唯一生产实现（ADR-0062）。

运行时策略重检: Host Function I/O 前比对 Cedar policy etag 与 Wasm 实例化时 `policy_etag_at_start`。etag 变更 → 重调 [Cedar-Gate] Review → FORBID 返回 `ErrPolicyRevoked`。etag 比对 O(1)，仅变更时触发完整评估。

**TOCTOU 防御**:
- **(a) 可逆操作**（`read_local` 等）: 接受窗口
- **(b) 不可逆操作**（`write_network` 等）: `PreCheck` → `I/O` → `PostCheck` 三重校验:
  1. **PreCheck**: `etag` + M8 Blackboard Lease Version 比对
  2. **I/O**: 执行工具调用（须尊重 `ctx.Done()`——M8 Reaper 回收任务前会 cancel agent context）
  3. **PostCheck**: 重新校验 `etag` + Lease Version。`etag` 变更 → 审计 `policy_etag_diverged_during_io(CRITICAL)` + 标记 `policy_stale`；Lease Version 不匹配（任务已被 Reaper 回收）→ 审计 `side_effect_orphaned(CRITICAL)` + 写入 M3 `decision_log`（非 events 表——任务所有权已转移，孤儿记录仅作审计轨迹，不参与 Blackboard 状态变更）
- **(c) 特权操作**（`privileged`）: 不走快捷路径，每次完整 `PolicyGate.Review`。M8 Reaper 重置任务前先 `cancel(agent.ContextCancel)` 并等待 5s 宽限期（M8 §1.7），工具须在 5s 内经 `ctx.Done()` 中止

配额隔离 per-instance（10000/5000/2000）；深度3 + 白名单 + per-instance = 三层纵深。

### 4.7 L3 策略监视器

L3 的 I/O 路径不经过 Wasmtime host function（syscall 由 VM 内核栈 / gVisor sentry 拦截），不触发 etag 重检，故需独立监视器。L3PolicyMonitor goroutine（每个 L3 sandbox 一个）:
- (a) 订阅 [Cedar-Gate] SubscribeEtagChange（channel 推送）
- (b) etag 变更 → 重评 CapToken: 允许 → 更新 etag；FORBID → 三级阻断: (1) 关闭网络出口（各平台方式见 `internal/sandbox/sandbox_impl.go`）；(2) context cancel M4（通知 Agent 策略吊销）；(3) 审计 `l3_policy_revoked_network_killed` (CRITICAL)
- (c) 毫秒级阻断，闭合 etag 变更到下一次 I/O 之间的逃逸窗口（最长 4min CapToken TTL）
- (d) L3 不可用时（Tier 0 / 平台后端不可用）不实例化

### 4.8 Remote Sandbox（Sbx-L4，可选非硬依赖）

实现 `internal/sandbox/remote_sandbox.go`（`RemoteSandbox`）+ `internal/sandbox/sandbox_router.go`（`WithRemote`/`RouteByTier`）；配置 `internal/config/config_types.go`（`RemoteSandboxConfig`，`[sandbox.remote]`）；装配 `cmd/polaris/boot_tools.go`（`Remote.Enabled` 且 `Endpoint` 非空时注入）。

**用途**：Tier-0 无法启动 L3、或需更高算力/隔离度时，将工具执行委托给远端 HTTP 执行器（自托管 VPS、E2B、Modal、Daytona 等），作为 L3 不可用时的可选降级路径。走**显式配置门控**（默认 `enabled=false`，需显式配置 `endpoint`）——用户代码/数据离开本机属成本/隐私权衡，非纯硬件问题（`[Tier-0-Limit]`，00-Global-Dictionary）。

**路由优先级**（`RouteByTier`）:
- 工具声明 `sandbox: remote`（`types.SandboxRemote`）→ 优先 `RemoteSandbox`；未注入时按 fallback 链降级 Wasmtime → Container → （不可信来源）fail-closed 拒绝
- `SandboxContainer` 请求在本地 `ContainerSandbox` 不可用且 `RemoteSandbox` 已注入 → 路由至远端

**安全约束**:
- 出站 HTTP 经 `network.NewSafeHTTPClient`（XR-06 SafeDialer），非旁路直连
- `AuthToken` Bearer 认证；配置留空时读 `POLARIS_REMOTE_SANDBOX_TOKEN`，避免密钥落盘
- 响应体读取上限 32MB（`io.LimitReader`）
- **不改变信任边界**：仍受 `AssignSandboxTier`/Cedar/CapabilityToken 前置校验，`mustIsolate`（`trustTier < TrustOfficial`）语义不变

**已知限制**：远端沙箱强度由第三方保证，HE-Rule-2 "可验证执行"在此退化为信任假设；成本计量与配额未接入 `internal/observability/budget`。

### 4.7 Sandbox-L4-Persistent（ADR-0008 决策三，可选，Tier2+）

`types.SandboxPersistent` 是真实可用的长程有状态 CodeAct 会话后端：session-scoped 长驻解释器进程池（非 CRIU/Firecracker checkpoint/restore——bwrap/Seatbelt 无对应原语）。默认关闭（`sandbox.l4_enabled=false`），Tier-0/1 不受影响。

**实现**（`internal/sandbox/sandbox_persistent.go` + `sandbox_persistent_session.go` + `sandbox_persistent_harness.go`）：
- `PersistentSandbox` 实现 `SandboxProvider`；`Available()` = `ArgvWrapper` 已注入且宿主 PATH 上有 python3/bash 之一。
- 每个 `SessionID` 一个长驻子进程（Python `python3 -u -c <harness>` 逐行 JSON 协议；Bash `bash --noprofile --norc -s` 哨兵 echo 协议），经 `ArgvWrapper`（`internal/sandbox/argv_wrapper.go`，实现 `internal/tool/sandbox` 的 `RustArgvWrapper`）取得与 L3/MCP stdio 长进程同一条 Rust FFI 沙箱封装——隔离强度与 L3 一致，不构成 inv_global_07 降级例外。
- 生命周期 `PersistentSandboxConfig{IdleTTL, MaxSessions, ExecTimeout, ReapInterval}`（默认 10min/8/30s/30s）：后台回收按 IdleTTL 终止空闲会话，超 MaxSessions 淘汰最久未用；协议层任何失败即判会话不可信并终止，下次调用拿全新会话。`Shutdown()` 由 `cmd/polaris/main.go` 优雅关闭时调用。
- `RouteByTier` 的 `SandboxPersistent`：`Available()` 时路由 L4，否则按 Container 同等降级链（Container → Remote → fail-closed）。
- `code_act.go Execute()` 在构造脚本**前**查询 `PersistentSandboxAvailable()`：可用则发送原始代码（长驻进程自持状态），否则走 `buildExecutableScript` 包装路径；`StatefulSession && SessionID!=""` 时 tier 覆盖为 `SandboxPersistent`。
- 配置 `M7ToolThresholds.SandboxL4Enabled`/`SandboxL4Backend`（固定 `"live_process_pool"`）/`SandboxL4IdleTTLSeconds`/`SandboxL4MaxSessions`/`SandboxL4ExecTimeoutSeconds`/`SandboxL4ReapIntervalSeconds`；`l4_enabled && Tier>=2` 才装配。

**已知边界**：沙箱边界（AllowedPaths/网络）在会话创建时固化；Python `input()` 等阻塞 stdin 挂起至 ExecTimeout 熔断；单会话同时只处理一次调用（`execMu` 串行）；未接入 OOM Guard，`MaxSessions`/`IdleTTL` 是唯一资源治理手段。

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

三层防线: 语义([Cedar-Gate])→数据([TaintLevel])→网络(SSRF)。安全闭环: [TaintLevel] [Taint-Prop]→[Cedar-Gate]→CapabilityToken→[Sandbox-L1/L2/L3]→RateLimit→[EventLog]

**trust_level 动态推导**: `InMemoryToolRegistry.ExecuteTool` 按 `tool.Source` 计算传给 PolicyGate 的 `trust_level`：Builtin → 4（系统信任），MCP/A2A → 2（社区信任），其余 → 1。

**`capability_token_valid`**：表示本次请求携带通过 `tokenVerifier.Verify` 的能力令牌（`ExecEnvelope.Execute` Step 1），与 `tool.Capability` 级别无关。Agent 路径在节点通过 S_VALIDATE 后 JIT Mint（MaxCalls=1、TTL 5min），经 `protocol.CtxCapabilityTokenKey` 注入该次调用、返回即撤销；S_VALIDATE L1 预检以"通过即签发"评估该条件（ADR-0098 决策五）。签发限定为本 Agent 最近一次通过 S_VALIDATE 的计划节点（工具名 + 参数字节一致），不在其中的调用（含未经 L1 校验的 Saga 补偿动作）拒签——令牌证明"调用属于已校验计划"，不是 trust<3 工具的独立审批。

### 5.3 Shadow Sink

**`write_network`/`privileged` 不可逆操作**:
1. **Phase1 Shadow Dry-Run**: 路由到 `localhost:{port}/_shadow/{tool_name}` → 基于 `output_schema` 自动生成 mock 响应 → Agent Kernel 验证 schema 一致性
2. **Phase2 HITL**: 展示工具名+参数摘要+Shadow结果 → approve生成一次性Token(TTL 见 `spec/state.yaml §m7_tool.dryrun_protect_window_seconds`) / deny拒绝
3. **Phase3 Real Execution**: 一次性Token签发 → 执行

`generateMockFromSchema`: string→"[MOCK:{name}]"；number→min/max中值；boolean→false；array→1元素；object→递归

### 5.4 DryRunMode

**`M7.DryRun(call)` 执行流程**:
1. **参数校验**: schema + 类型 + 范围
2. **权限检查**
3. **目标存在性**: 本地 stat / 域名白名单 + SSRF CIDR / recipient 格式
  > **禁止真实网络请求**: TCP dial / HTTP HEAD / DNS
4. **副作用预估**: bytes / rows / cost
5. **返回**: `DryRunResult{Feasible, Warnings[], EstimatedImpact, Reason}`
  - feasible + 无 warning → 自动执行
  - feasible + 有 warning → HITL
  - not feasible → error

DryRun结果→[EventLog]（tool.dry_run_result），Reflexion回顾。

---

## 6. Capability Token

短寿命权限令牌，沙箱门口 JIT 签发。实现 `internal/action/capability_token.go`。

**JIT Minting**: 1 Planner 决定调用工具 → 2 生成 ToolIntent（不签发）→ 3 M8 Blackboard CAS 认领 → 4 HITL 审批（可能 10+ 分钟）→ 5 Worker 进入沙箱 Gate → 6 Gate1-5 全过 → 7 JIT Mint (`MaxCalls=1, TTL=5min`) → 8 沙箱执行。审批期间无有效 Token 泄露。

**续期**: 长任务（预估 >4min）宿主 goroutine 每 (TTL-60s) 续期，校验：Agent Lease 未过期；比对 Cedar-Gate etag，变更则 PolicyGate.Review 重评——允许 → 新 Token + 更新 etag；FORBID → 取消沙箱 + 审计 `token_renewal_policy_revoked` (CRITICAL)。默认 MaxRenewals=5（30min 窗口）；长程覆盖: compile=10 次/60min, crawl=8 次/48min, index=8 次/48min。

**任务派生深度**: 委托链已移除（§4.6），深度限制由 M8 `orchestrator.MaxSpawnDepth=3` 在任务派生层强制。

---

## 7. 动作空间扩展

### 7.1 Computer Use / GUI 自动化架构

`computer_use` 通过独立外部 MCP 插件/微服务（如 Computer MCP Server）接入，不再是硬编码系统内置侧车，保证主干 Agent 进程安全与生命周期解耦。严禁在外部扩展内嵌核心 AI 模型（OmniParser / VLM 均留在主干推理网关）。

- **感知**: `xcap` 跨平台截屏；语义 UI 树直调 OS 原生 API（Win `uiautomation-rs`、Mac `axuielement`、Linux `AT-SPI2`）
- **执行**: `enigo` 键鼠注入；Linux 兜底用 `evdev` 写 `/dev/uinput`（弃用实验性 Wayland `libei`）
- **模式**: ForegroundIntent → physical（依赖 Computer MCP 插件）；BackgroundTask/AutoCurriculum → headless（Tier 1+，`Xvfb :99 -screen 0 1280x800x24`）
- **耗时追踪**: 底层追踪表（`decision_log` / `agent_actions`）须录入 `created_at` 与 `updated_at`
- **GUI Action Loop**: `see → decide → act` 循环 `maxSteps` 次：Capture + UITree → VLM DecideAction（主干）→ MCP Command → executeAction → GUIResult

**HITL 拦截门控**（`interceptComputerUse`，`internal/agent/agent_execute_util.go`）：触发工具 `computer_use`、`browser_use`；受全局 `permission_mode`（`PreferencesRepo.GetPermissionMode`）控制：`full_access` 自动放行（依赖 Cedar 最终防线）/ `auto_review`（默认）仅危险动作触发 HITL / `default` 全部触发。危险动作：`computer_use` 的 `key/type/left_click/right_click/double_click/left_click_drag`；`browser_use` 的 `click/type/key`。HITL 路径 `a.hitl.Prompt()`，deny 返回 `CodeForbidden`（任务中止）。

### 7.2 LAM（Large Action Model）— ComputerUseEngine

`internal/action/lam/lam.go`（`ComputerUseEngine`，实现 `LargeActionModel`）：`intent + ScreenState → VLM → computerUseArgs JSON → ExecutorFn`。

- **硬件门控**: `FeatureComputerUseGUI`（`internal/observability`）在 `ExecuteAction` 入口检查，未通过返回 `ToolResult{Success:false}`，不抛错。
- **双路径**: Tier 0 / DOM-only（screenshot 为空或 >2MB）只送 DOM 文本，零图片 token；Tier 1+ / vision（≤2MB）额外带分辨率并把 base64 截图注入 `protocol.Message.Parts`。
- **VLM 响应** (`vlmActionOutput`): `action`（screenshot|left_click|right_click|mouse_move|type|key）、`coordinate [x,y]`、`text`、`reasoning`（仅日志，不转发 executor）。
- **ExecutorFn 注入**: 由调用方注入（通常 `action.NewComputerUseTool().Execute`），解耦 `internal/action/lam` 与父包防循环依赖；`executor=nil` → dry-run，返回解析的动作 JSON。当前 boot 以 `sb.Router` 作 VLM provider、executor 为 nil。
- **接线状态**: `ExecuteAction` / `StreamBus` 无生产调用入口（`internal/tool/builtin` 无 computer_use 工具，Agent 仅经 `lamPolicyAdapter` 用 `CheckPolicy`）；GUI 执行器落地前刻意不接线，接入须同时提供 builtin 工具与 executor。
- **Agent Kernel 集成**（ADR-0025）: `CheckPolicy(ctx, actionJSON)` 由 `Agent.interceptComputerUse` 在 HITL 审批**前**调用，实施 Cedar `browser_automate/lam/{allow_net:true}` deny-by-default 预检；`lamEngine==nil` 时跳过（nil-safe）。顺序：Cedar PolicyGate（快速拒绝）→ HITL。
- **LAMConfig**: `Enabled`；`PerceptionMode`（`auto` 按内存降级 / `local_omniparser` / `cloud_vlm`）；`ResolverModel`（空串 = 由 InferenceRouter 按图像 Part 选 Vision Provider）。
- **ActionDiscretizer**（`internal/action/lam/continuous_action.go`）: 余弦相似度离散化（`Discretize`），连续向量 → 离散工具调用，~1-5ms。

### 7.3 StreamingActionBus（CANONICAL SOURCE）

`internal/action/lam/streaming_action_bus.go` 的 `StreamAction()`：步数检查（超 maxSteps=1000 报错）→ 令牌桶速率控制（阻塞至取得令牌或 ctx 取消）→ ActionClipper 向量钳制（可选）→ `DisplayServer.SendAction`（nil 时 no-op 安全降级）。类型校验与 ASCII 白名单为设计目标，当前 `StreamAction` 不对 `ActionType` 做枚举约束，由调用方保证。

DisplayServer：Xvfb 适配已实现（`display_server_xvfb.go`，依赖 `xdotool`/`xwd`/`convert`），`cmd/polaris/boot_agent.go` 探测接入——`FeatureVisionDisplayServer`（MinTier1，linux）满足且 `lam.XvfbAvailable()` 通过才装配，否则 nil no-op；VNC/Wayland 适配无实现。

M4 S_EXECUTE 路由: tool_call→ActionDiscretizer→ToolCall；mouse_delta / key_sequence→StreamingActionBus→虚拟帧缓冲；其他→ErrUnsupportedActionType。工具调用走离散化（~1-5ms），GUI 走总线（<0.5ms 背压），混合按 Type 分流并行双通道。

Security: StreamingActionBus 不绕过 Capability——mouse_delta/key_sequence 需 CapGUIAutomation（Tier1+ 默认启用，Tier0 需显式授权）。

### 7.4 `[CodeAct]` — 即时代码执行行动空间

区别于 [Logic-Collapse]（沉淀型 Python 技能，走 staging 后入 Skill Library）：CodeAct 是 **ad-hoc 一次性代码 + 立即执行**，不沉淀（不写 Skill Library、不走 Staging/Eval Gate），共用 ContainerSandbox 路径。M4 S_EXECUTE 在"组合多工具 + 中间计算 + 条件分支"时可选。

**`inv_global_07` 强制约束**（无豁免）:
- Source: `ToolSource=LLMGenerated`
- Sandbox: 强制 Level≥3（ContainerSandbox），`policyGate` + `sandbox.Level()` 双重门控，任一不满足立即 fail-closed
- Capability Token: 由调用方经 `req.CapabilityID` 传入（CodeAct 不自行 Mint）
- Cedar: deny-by-default，需 `permit code_act when ... approval_status == "approved"`；`policyGate` 未注入 fail-closed
- Audit: 完整代码 + stdout + exit_code，经 `toolExec.RecordAudit(ctx, "code_act", ...)` 写 EventLog

**`Execute`**（`internal/action/codeact/code_act.go`，已接线）: 语言校验（python|bash）→ CapabilityID 非空 → Cedar 评估（fail-closed）→ sandbox.Level()≥3 断言 → `sandbox.Run` → `CodeActResult`；输出端 Post-Execution PII 脱敏。`boot_server.go` 在 `FeatureL3Sandbox` 启用时构造引擎并注入 `agent.SetCodeAct()` / `httpServer.SetCodeActEngine()`。代码上限 `MaxCodeSizeBytes`（默认 16384，`WithMaxCodeSize`）。污点门控由 Cedar 评估 + Level≥3 断言间接覆盖。

**REPL 状态保持**（`internal/action/codeact/code_act_stateful.go`）: `StatefulSession=true`（默认 false）且带 SessionID 时，同 SessionID 的连续调用延续变量状态。L4 可用（§4.7 Sandbox-L4）时由长驻进程自持状态；否则脚本前后注入样板（Python pickle 快照 `globals()` 到按 SessionID 命名的状态文件，Bash `declare -p` 导出再 `source`）。L0 AST / L1 GovernanceAgent / L2 LLM 评审（§7.5）作用于用户原始代码，早于样板注入；SessionID 经路径净化后才用于文件命名。

| 维度 | CodeAct | Logic Collapse Python 技能 (M6) |
|------|---------|--------------------------------|
| 沉淀 | 否（一次性） | 是（Skill Library，HE-6） |
| Staging | 不走 | 完整 7 阶段（ValidatePython + Eval Gate + 签名）|
| Sandbox | L3 ContainerSandbox | L3 ContainerSandbox |
| 执行路径 | `stageScript`（VFS `StageEphemeralFile`，未接入时降级 `writeTempScript`）→ sandbox.Run | SkillRegistry 读 skill.py → sandbox.Run |
| 触发 | M4 显式选择 | M9 自动后台（≥50 成功轨迹）|
| HT0 可用 | 否（L3 依赖）| 否（L3 + FeatureLogicCollapse 双门控）|

**M4 S_EXECUTE 决策树**: 单工具 → 标准 tool_call；组合 ≥3 工具 + 中间计算 → 候选 CodeAct（询问 LLM 偏好 + Cedar 校验）；高频可复用 → 后台 Logic Collapse / Auto-Curriculum 沉淀。

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

Logic Collapse (M6) 创建新技能；本机制提升已有工具的使用策略（从调用轨迹学最优参数与失败模式）。

### 8.1 与 Logic Collapse 的分工

| 维度 | Logic Collapse (M6) | Tool Usage Policy Evolution (M7) |
|---|---|---|
| 进化对象 | 创建新技能 | 已有工具的使用策略 |
| 触发 | 50+ 成功 | 每次调用后更新统计 |
| 输出 | Python 技能（src/skill.py）+ SKILL.md | 策略提示词 + 参数建议 |
| 粒度 | 工具级 | 调用级 |

### 8.2 策略模型

持久化类型见 `internal/action/tool_usage_policy.go`：**ToolUsagePolicy**（`ToolName` / `ParamHints map[string]ParamHint` / `BestFor` / `NotRecommendedFor`）、**ParamHint**（`DefaultValue` / `Description` / `MinValue` / `MaxValue`）。

PolicyEvolver（§8.3）运行时维护、不单独持久化的数据；重启后由 `WarmStart` 从 `session_trajectory`（045）每工具最近 window 条工具事件重放恢复（Params 不还原，ADR-0104 决策七）:
- **FailurePattern**: 失败签名（ErrorType × 输入特征）+ 频率 + LLM 生成的缓解策略
- **CoToolPattern**: 工具组合（ToolName × Relationship ∈ {before, after}），按频率排序
- **运行时统计**: `SuccessRate`（加权平均）、`AvgLatencyMs`、`UseCount`

### 8.3 PolicyEvolver

`internal/action/tool_usage_policy.go`：
- `WarmStart(outcomes)`：启动期按时间序重放历史结果进滑动窗口（与 `RecordOutcome` 同一写入路径）
- `RecordOutcome`：SuccessRate 滑动窗口加权更新 + 失败模式提取（连续 3 次同类失败自动生成缓解建议）
- `GetContextHint(toolName)`：历史调用 ≥20 次返回 ParamHint 建议 + 高频失败警告，否则空（冷启动不注入噪声）
- `BuildSystemHintBlock()`：聚合已激活工具提示为 `<tool-hints>...</tool-hints>`，M4 DAG 节点执行前注入 System Prompt ZoneMutableSkill 区；无提示返回空串。不修改工具定义和 schema

启用条件：单工具 ≥20 次历史调用自动激活；Tier 0 低频下用冷启动默认值。

---

## 12. 降级与失败模式

| 故障场景 | 降级路径 | 恢复策略 |
|---|---|---|
| 沙箱启动失败 (L2 Wasm / L3) | 拒绝执行该工具 + [EventLog]/AuditEvent | 重启沙箱宿主 / 重装后恢复 |
| Capability Token 校验失败 | 拒绝执行 + AuditEvent | 重新申请 Token |
| MCP 外部 server 不可达 | mark_unreachable → 工具从可用列表移除 | 心跳恢复后重新注册 |
| Rate Limiter 触发 | 429 排队 + Retry-After | 窗口过期自动恢复 |
| 不可逆操作 DryRun 失败 | 拒绝执行 + HITL 升级 | 手动审批后签发一次性 Token |
| L3 平台后端不可用（Linux 无 KVM 回退 gVisor；macOS 旧系统 / Windows 无 WSL2） | L3 不可实例化 → ErrFeatureUnavailable | 平台条件满足后恢复 |

与 OSMemoryGuard 协同: L1 预警 → 禁止启动新 Wasm 实例 / L2 紧急 → kill 空闲沙箱 / L3 临界 → kill 全部非关键沙箱，仅保留当前交互任务。

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m7_tool`。

## 13. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces_*.go` + `types.go`。本表仅列依赖方向 + 一句话语义 + 锚点。

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

## 14. Plugin Registry（ADR-0103）

End-User 可通过 Plugin Bundle（tar.gz）分发技能 + MCP 组合，无需改源码。决策见 [ADR-0103](./decisions/ADR-0103-extension-dual-vendor-standard.md)（原 ADR-0016/0015）与 M13-bis §5.3。

**Plugin manifest**（`plugin.json`，`PluginBundleManifest`）字段：`name` / `version` / `description` / `mcp_inline`（server 名 → command+args）/ `mcp_servers`（`.mcp.json` 路径）/ `skills[]`（`path`、`name`）。bundle 目录下还可含外部厂商格式（`ai-plugin.json` / `plugin.toml` / `skills.yaml`），由 `internal/extension/pluginspec` 唯一解析器解析。

**安装路径**：`~/.polaris/extensions/plugin/{ext_id}/`（HTTP tar.gz 下载解压）。

**加载流程**：`POST /v1/plugins/install` → `internal/gateway/server/plugin/catalog_download.go` `downloadAndInstallExtension()` → 解析 `plugin.json` → `mcp_inline` / `.mcp.json` 经 `installBundleMCP()` 写 `mcp_servers` + `MCPManager.Add()`；`skills[]` 经 `installBundleSkill()` 写 `skills`（runtime=script）→ INSERT `plugins`（021）写 bundle 元数据。

**安全约束**：所有子路径经 `safeJoin()` 校验，防 bundle 内路径穿越；Plugin Bundle MCP 默认 Taint=High（inv_M7_02）；Script Skills trust_tier 继承 extension_catalog。

**代码位置**: `internal/gateway/server/plugin/catalog*.go`（HTTP 入口）、`internal/extension/marketplace/catalog_{sync,install,versions}.go` + `sources.go`（同步 / 来源取回 / 依赖安装）、`internal/extension/pluginspec`（唯一解析器）。

---

## 15. Hook 框架（hooks.json，ADR-0103 决策六）

两家共同的 `hooks.json` 模型取代原 `hooks.yaml` 工具事件引擎与 `[ShellHooks]`（见 `00-Global-Dictionary.md` `[Hooks]`）。

**模型**：`internal/action/hook` —— `hook.go` 事件/处理器/配置解析；`registry.go` 来源快照（`SourceProvider` 注入，`Reload` 原子替换）；`io.go` 标准输入与输出决策合并（退出码 0/2/其他、`continue`/`decision`/`hookSpecificOutput.{permissionDecision,updatedInput,additionalContext}`，deny > ask > allow）；`runner.go` 分发（事件级 PolicyGate、PII 脱敏、同步并发 + async 后台）；`exec_command.go`（shell / exec 形式、`${CLAUDE_PLUGIN_ROOT}` 等路径变量、`CLAUDE_PLUGIN_OPTION_*`，shell 形式禁止 `${user_config.*}`）；`exec_remote.go`（http 经 SafeDialer、`allowedEnvVars`；mcp_tool 模板替换；prompt/agent 经 Provider 路由判定 `{"ok":…}`）；`filter_if.go`（`if` 规则 `Tool(glob)`）；`firer.go`（实现 `sandbox.HookFirer` 与 `hitl.PermissionHooks`）。

**来源**：`extension/lifecycle/hook_sources.go` 组装用户级 / 项目级 hooks.json 与已启用插件的 hooks；信任按定义哈希存 `hook_trust`；`GET /v1/hooks`、`POST|DELETE /v1/hooks/trust` 审阅管理；插件安装/卸载/启停与信任变更后刷新快照。

**触发点**：

| 事件 | 触发点 |
|------|--------|
| `PreToolUse` | `ExecEnvelope.Execute` Step 1.5：veto-only；`updatedInput` 改写后重新执行 PolicyGate；`ask` 按拒绝处理（无逐次交互通道） |
| `PostToolUse` / `PostToolUseFailure` | `ExecEnvelope.Execute` 执行后同步触发；`decision:block` 原因与 `additionalContext` 以 `<hook-feedback>` 追加到工具输出并强制 TaintHigh |
| `SessionStart` / `UserPromptSubmit` | `session` 编排入口（交互式与 Headless）；阻断即拒绝本轮，附加上下文以 `<hook-context>` 随任务意图下发 |
| `Stop` / `StopFailure` | 回合结束：`decision:block` 以原因续跑，最多 3 次（`stop_hook_active` 标记）；空响应触发 StopFailure |
| `SessionEnd` | `/clear`（随后 SessionStart:clear）与删除会话 |
| `PreCompact` / `PostCompact` | `CompressionService.compact`（trigger manual / auto；阻断跳过压缩） |
| `PermissionRequest` / `Notification` | `hitl.GatewayImpl.Prompt`：deny 直接拒绝（allow 不代替人工批准）；审批发起时发 Notification |
| `Interrupt` | 用户中断接口 |

Hook 输出强制 `TaintLevel=High`，经 PolicyGate 决定注入，禁止进入 Immutable Zone。

**执行**：命令处理器经 `sandbox.RunStdio`（`ArgvWrapper` → Rust 沙箱封装 argv；stdin 注入、stdout/stderr 分离、退出码、超时与输出上限），封装失败拒绝执行。
