# ADR-0079: Sandbox-L4-Persistent 改用长驻解释器进程池（推翻 ADR-0078 的诚实占位结论）

## 状态
Accepted（已执行）

## 背景

ADR-0078 把 D4（原 GD-14-003）Sandbox-L4-Persistent 交付为"接线到位、后端诚实
留空"：`PersistentSandbox.Available()` 恒定返回 `false`，理由是原始设计设想的
CRIU（Linux checkpoint/restore）或 Firecracker microVM snapshot，在本仓库的
L3 沙箱架构（ADR-0008/ADR-0011 已收敛为 bwrap/Seatbelt 进程级隔离，废弃容器/
虚拟化路径）下没有对应的操作系统原语可用，且无法在当时的环境里对 CRIU 做
端到端真实验证。

复核后发现：D4 的真实动机（`internal/action/codeact/code_act_stateful.go`
`StatefulSession` 的既有局限）是"长程有状态 CodeAct 会话每次调用都重新起
一次性进程，用 pickle（Python）/`declare -p`（Bash）序列化状态，文件句柄/
线程/数据库连接等不可序列化对象被静默丢弃"。CRIU/Firecracker checkpoint/
restore 只是达成"状态不因重新起进程而丢失"这个目标的**一种手段**，不是目标
本身。ADR-0078 把手段和目标绑在一起，导致在手段不可行时直接判定整个能力
不可行——但还有另一种手段同样能达成目标，且不依赖任何本仓库缺失的操作系统
原语：**让解释器进程在多次调用之间根本不退出**。进程不退出，其内存里的
变量、已打开的文件句柄、线程、数据库连接自然原样保留，不需要序列化、也就
不存在"pickle 无法序列化"的问题——这与 Jupyter Kernel、大多数托管代码执行
产品（E2B、Modal 等）事实上采用的机制一致。

## 决策

**推翻 ADR-0078 关于"Available() 必须恒定为 false"的结论**，用 session-scoped
长驻解释器进程池替换诚实占位实现，`types.SandboxPersistent`/
`PersistentSandbox` 这个 tier/类型沿用不变（ADR-0078 调研阶段已确认这正是
预留给此类能力的落点）。

1. **隔离强度不降级**：会话进程通过新增的消费方接口
   `internal/sandbox.ArgvWrapper`（`WrapArgv(ctx, protocol.SandboxContext)
   (*protocol.WrapArgvResult, error)`）取得 Rust FFI（`native_sandbox_wrap_argv`）
   封装后的 argv/env，与 L3 `ContainerSandbox`、MCP stdio 长进程
   （`internal/extension/mcp/mcp_client.go buildSandboxedMCPCmd`）走同一条
   底层沙箱封装机制（bwrap/Seatbelt），只是由 Go 侧自己 `exec.Command` 并
   长期持有 stdin/stdout 管道，而不是像 `CmdRunner` 那样运行到完成才返回。
   `ArgvWrapper` 在 `internal/tool/sandbox`（`RustArgvWrapper`）实现并由
   `cmd/polaris` 装配注入，打破 `internal/sandbox` ↔ `internal/tool/sandbox`
   的包循环，与既有 `CmdRunner`/`WrapBashCmdRunner` 完全同构的模式
   （R1.4 消费方接口 + 组合原语）。不满足 inv_global_07"禁止降级隔离"的
   例外——L4 与 L3 隔离强度一致，只是进程生命周期更长。

2. **协议**：
   - Python：长驻进程运行一个极薄的"harness"脚本（`python3 -u -c
     <pythonSessionHarness>`），逐行读取 `{"code": "..."}` JSON 任务，用
     显式 `globals` 字典 `exec()`，`contextlib.redirect_stdout/stderr`
     捕获输出，回写一行 JSON 响应。用显式 exec/JSON 协议而非把用户代码喂给
     `python3 -i` 交互式 REPL，是为了避免解析 REPL `>>>`/`...` 提示符和
     "空行结束多行块"规则的脆弱性——JSON 单行响应天然定界，不依赖对输出流做
     启发式解析。
   - Bash：长驻 `bash --noprofile --norc -s`（非交互脚本模式读 stdin，天然
     保留变量/函数/cwd，进程不因处理完一批命令而退出），每次调用在代码末尾
     追加一行哨兵 `echo "<<<POLARIS_END:<uuid>:$?>>>"`，读到哨兵行为止，从中
     解析退出码。stdout/stderr 合并到同一管道。
   - 任一协议层面失败（写入失败/读取失败/响应格式错误/超时）都判定整个会话
     协议已不可信，立即终止该会话；下一次调用会拿到全新会话，不允许一个
     "错位"的旧会话带病继续使用。

3. **生命周期管理**：`PersistentSandboxConfig{IdleTTL, MaxSessions,
   ExecTimeout, ReapInterval}`（默认 10 分钟空闲回收 / 8 会话上限 / 30 秒
   单次调用超时 / 30 秒回收扫描周期）。后台回收 goroutine 用
   `pkg/concurrent.SafeGo` 启动；超过 `MaxSessions` 时淘汰最久未使用的会话；
   `Shutdown()` 终止全部存活会话，由 `cmd/polaris/main.go` 优雅关闭时调用
   （nil-safe，未开启 L4 时是空操作）。

4. **CodeAct 集成**（`internal/action/codeact/code_act.go`）：`Execute()` 在
   构造脚本内容**之前**同步查询 `ca.envelope.PersistentSandboxAvailable()`
   决定路径——可用时发送原始代码（长驻进程自己保有状态，不需要 GD-4-002 的
   pickle/env 包装样板；套在一个从不重启的进程上没有意义），不可用时维持
   `buildExecutableScript` 既有包装路径不变（零回归）。`ExecEnvelope.Execute`
   在 `StatefulSession && SessionID != ""` 时把 `actualTier` 覆盖为
   `types.SandboxPersistent`（`AssignSandboxTier` 是面向所有工具的通用判定，
   不了解"有状态会话"这一 CodeAct 专属语义）。

5. **配置**：`M7ToolThresholds` 新增 `SandboxL4IdleTTLSeconds`/
   `SandboxL4MaxSessions`/`SandboxL4ExecTimeoutSeconds`/
   `SandboxL4ReapIntervalSeconds`；`SandboxL4Enabled`（默认 `false`）/
   `SandboxL4Backend`（诊断标签，固定为 `"live_process_pool"`）沿用
   ADR-0078 已有字段名不变。`sandbox.l4_enabled && hwTier>=2` 才装配，
   Tier-0/Tier-1 行为零回归。

## 已知边界（如实记录）

- 沙箱边界（`AllowedPaths`/网络策略）在会话首次创建时固化，同一 SessionID
  后续调用无法更改——bwrap/Seatbelt profile 在进程启动时确定，长驻进程期间
  不能重新配置。
- Python 用户代码内调用 `input()` 等阻塞式 stdin 操作会挂起该次调用直至
  `ExecTimeout` 熔断（stdin 被协议独占）。
- 单会话同一时刻只能处理一次调用（`execMu` 串行化），同一 SessionID 的并发
  调用会排队而非并行执行。
- 空闲回收依赖 `ExecTimeout` 显著小于 `IdleTTL` 这一软约束（默认
  30s vs 10min）防止回收器在一次正常执行的中途误判为空闲；未做强校验，
  依赖默认值配比合理，运营者自定义配置时需自行保证这一关系。

## 与 ADR-0078 的关系

本 ADR **推翻** ADR-0078 关于"CRIU/Firecracker 不可行 ⇒ L4 整体不可行，
Available() 恒为 false"的结论，**保留**其安全分析（bwrap/Seatbelt 无
checkpoint/restore 原语、CRIU 需要额外 namespace 工程、伪造后端违反 HE-2）——
那部分分析本身没有错，只是"L4 必须是 checkpoint/restore"这个前提是可以
松动的。同时保留 ADR-0078 顺带订正的 `pkg/types/enums_tool.go`
`SandboxContainer` 过时注释修复。

## 后果

- D4 从"接线但不可用"变为真正可用：长程 CodeAct 会话的文件句柄/线程/数据库
  连接等此前被 pickle 静默丢弃的状态，现在因为进程根本不重启而原样保留。
- 新增 `ArgvWrapper` 消费方接口/`RustArgvWrapper` 适配器可被未来其他需要
  "长驻进程 + 手动持有管道"的场景复用（当前场景：MCP stdio 已经用同一底层
  Rust 能力自行实现了一遍，未来可评估是否收敛到共享 `ArgvWrapper`，本 ADR
  不强制，留作独立评估）。
- 不引入 Docker/CRIU 依赖，不违反 ADR-0008/ADR-0011。
- 代价：单会话进程常驻期间持续占用一份解释器内存（Python 进程 ~10-30MB
  起步），`MaxSessions` 上限和 `IdleTTL` 回收是唯一的资源治理手段；高并发
  多会话场景下的内存水位需要运营者结合 `SandboxL4MaxSessions` 与主机 Tier
  自行评估，未接入 `internal/observability/probe` OOM Guard 联动（留作未来
  独立评估）。
