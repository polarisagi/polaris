# Polaris 全局公共字典

> **一句话定位**：所有模块文档的唯一权威概念定义源。子模块文档中出现的 `[Concept]` 标签均指向本文档对应条目。
>
> **代码位置**：无特定（全局字典）

---

## §0 术语命名空间前缀

避免 L0/L1/L2/L3 等标识符在架构层次、记忆层、沙箱级、演化级四个维度过载。全局统一使用以下前缀：

| 维度 | 前缀 | 值范围 | 用途 |
|------|------|--------|------|
| 架构层次 | `Arch-L` | Arch-L0 / L1 / L2 / L3 | 基础设施 / 认知 / 协同 / 治理 |
| 记忆层 | `Mem-L` | Mem-L0 / L1 / L2 / L3 | Working / Episodic / Semantic / Procedural |
| 沙箱级 | `Sbx-L` | Sbx-L1 / L2 / L3 | InProc / Wasmtime / microVM |
| 演化级 | `Evo-L` | Evo-L0..L4 | 配置 / prompt / skill / 策略 / 源码 |
| Hardware Tier | `HT` | HT0..HT3 | 8 / 16 / 24 / 64 GB |
| Task Priority | `Priority-` | 0..3 | 用户交互 / 前台辅助 / 后台优化 / 最低 |
| Model Pool | 具名 | — | `general` / `default` / `reasoning`（代码实际值；详见 M1 §4.2） |

子文档首次出现时使用全名（如 `Mem-L1 Episodic`），后续上下文清晰时可缩略。代码中标识符按语言惯例（如 `MemoryLayerEpisodic`），不使用前缀。

---

## §1 系统硬约束

### [Tier-0-Limit]
8GB 内存硬上限。核心路径必须在 8GB 内完整运行。超出能力通过硬件门控解锁，不作硬依赖。

### [Tier-1-Limit]
16GB 内存。解锁更大 Agent 并发。

### [Tier-2-Limit]
24GB+ 内存。解锁 gVisor L3 sandbox、更大规模知识图谱。Linux 环境可选 Firecracker microVM 升级（最高隔离级别，需硬件 KVM）。

### [Tier-3-Limit]
64GB+ 内存。解锁全能力（包括本地大模型推理、梯度训练 QLoRA，超高配置电脑专享）。

### [Day0-ColdStart]
全模块强制要求：空索引/空数据库不报错，返回确定性降级行为。禁止因缺少数据而 panic 或阻塞启动。

### [Phase0-Bootstrapping]
严格 L0 → L1 → L2 → L3 自底向上引导顺序。上层模块启动时检查下层模块健康状态，未就绪则阻塞等待。

### [Operator-Developer]
Fork 源码、参与贡献的技术用户。可直接修改 Go/Rust 代码。扩展行为直接落在源码层，无需 Hooks 机制。

### [End-User]
下载二进制或 Docker 镜像自托管的普通用户。通过 YAML 配置 + Web UI 驱动，**不修改 Go 源码**。其扩展边界：
1. Skills（Wasm）：LLM 主动调用的能力扩展
2. Shell Script Hooks（见 `[ShellHooks]`）：生命周期事件自动触发
3. MCP 工具：配置文件声明外部工具
4. `configs/*.yaml`：运行时参数

### [ShellHooks]
Shell Script Hooks — End-User 级生命周期扩展机制。类 git-hooks 模型，零依赖，脚本可用任意语言编写。

**目录**：`~/.polarisagi/polaris/hooks/`（或 `POLARIS_HOOKS_DIR` 环境变量覆盖）

**事件点**：

| 文件名 | 触发时机 | 环境变量 |
|--------|----------|----------|
| `gateway.startup` | 服务完全启动后 | `POLARIS_VERSION`, `POLARIS_WORKSPACE` |
| `session.new` | 用户执行 `/new` 或 `/reset` 后 | `POLARIS_SESSION_ID`, `POLARIS_PREV_SESSION_ID` |
| `message.before` | 处理用户消息前（非零退出 = 拦截） | `POLARIS_MESSAGE`, `POLARIS_SESSION_ID`, `POLARIS_CHANNEL` |
| `message.after` | AI 回复发出后 | `POLARIS_REPLY`, `POLARIS_SESSION_ID`, `POLARIS_CHANNEL`, `POLARIS_USER_ID` |
| `turn.stop` | Agent 完成本轮回复（对应 ADR-0015 §2.2 Codex Stop 语义），与 message.after 同点触发但语义独立，供未来分化 | `POLARIS_SESSION_ID`, `POLARIS_CHANNEL`, `POLARIS_USER_ID` |
| `session.compact.before` | 上下文压缩开始前 | `POLARIS_SESSION_ID`, `POLARIS_TOKEN_COUNT` |
| `session.compact.after` | 上下文压缩完成后 | `POLARIS_SESSION_ID`, `POLARIS_TOKEN_BEFORE`, `POLARIS_TOKEN_AFTER` |

**实现约束**：
- 脚本不存在 → 静默跳过，不报错
- 超时：非 `before` 类 5s，`before` 类 2s（阻塞主流程）
- `before` 类：非零退出 = 拦截并返回错误给用户；脚本 stdout 作为拦截原因
- 非 `before` 类：fire-and-forget，失败只记日志不影响主流程
- 安全：脚本在用户进程权限下运行，不注入 capability token

---

## §1-bis 全局数字公理（Digital Axioms）

以下公理独立于具体模块，定义全系统统一的状态、流程和风险语义。任何模块定义的状态枚举不得与以下公理冲突。

### 公理一: 生命周期时间箭头 (Lifecycle Arrow)

所有具有生命周期语义的字段（任务状态、事件状态、审批状态等）统一遵循以下因果时间箭头。禁止各模块自行定义含义不同的状态值。

| 值 | 语义 | 描述 |
|----|------|------|
| 0 | Pending | 起点 —— 数据入库/任务发布，等待处理 |
| 1 | Processing | 过程中 —— 正在消耗算力、等待外部回调或待人审批 |
| 2 | Completed | 正向终态 —— 成功完成，误差收敛 |
| 3 | Failed | 负向终态 —— 失败/熔断/拒绝，不可恢复 |
| 4 | Suspended | 挂起态 —— 高惊奇度寻觅，脱离主流程等待人工介入 |

适用举例：M8 TaskStatus（Pending/Claimed/Executing→Completed/Failed/Suspended），M4 TaskStatus, M2 OutboxStatus。

### 公理二: 污点与风险阶梯 (Taint & Risk Escalation)

数据的安全置信度按照 [TaintLevel] 五级定义（§4）。工具的风险等级按照 RiskLevel 四级定义。两者均为只升不降的单调阶梯。

### 公理三: 拓扑分类 (Taxonomy)

仅用于 Index 区分，无递进关系。各模块可按需扩展枚举值，但禁止与已有值语义冲突。

适用举例：M5 memory_layer（episodic/semantic/procedural），M10 entity_type（Person/Project/Tool/Concept 等）。

---

## §1-ter 跨模块交互规则

以下规则定义了模块间协作的硬性协议。它们不是 Interface 或 DDL 能强制表达的，但所有模块必须遵守。

| 规则 ID | 规则描述 | 影响模块 |
|---------|---------|---------|
| XR-01 | KillSwitch 阶段变迁由 M11 唯一触发。M3 推送 TokenBurnRate 至 M11 KillSwitch FSM（唯一触发路径）。M4/M8/M13 仅读取 `polaris_killswitch_stage` Gauge 并响应，不独立触发阶段变迁。 | M3→M11→M4,M8,M13 |
| XR-02 | SurpriseIndex 完整版由 M9 计算（依赖 MEMF），推送至 M3 Gauge。M3 内置基础版计算器（两组件，无 MEMF 依赖）作为 M9 不可用时的回退。M4 优先读完整版 → staleness >60s 回退基础版。 | M9→M3→M4 |
| XR-03 | TaintLevel 只升不降（output = max(inputs)）。受控降级仅通过四种 Sanitizer（见 §4 [Taint-Sanitizer]）。SanitizeBySchema 对字符串字段要求 format/pattern/enum/const 内容约束，裸 {"type":"string"} 不降级。 | M11→M4,M7,M10 |
| XR-04 | **DB 写路径三层规范**。见下方 [XR-04 补充](#xr-04-补充) | M2 全模块 |
| XR-05 | 自进化输出（M9 PromptOptimizer、M6 Logic Collapse）在合并到系统前必须通过独立 LLM-as-Judge 安全审查。审查使用与生成不同的 Provider 模型。 | M9→M11→M5, M6→M11→M5 |
| XR-06 | 所有出站网络连接强制通过 M11 SafeDialer.DialContext。禁止裸 net.Dial / grpc.Dial / http.Get。CI safe_dialer_lint 扫描违规 → ERROR。 | M11→M1,M7,M10,M13 |
| XR-07 | Tier 0 内存硬上限 8GB。OSMemoryGuard (M3) 与 ResourceGovernor (M13) 共享三级降级阈值（L1: 1.5GB, L2: 1.0GB, L3: 512MB）。任一组件触发降级即执行。 | M3,M13→全模块 |
| XR-08 | **日志规范**。见下方 [XR-08 补充](#xr-08-补充) | 全模块 |
| XR-09 | **LLM 调用规范**。见下方 [XR-09 补充](#xr-09-补充) | M1→M4,M9,M10 |
| XR-10 | **工具/技能/插件执行规范**。见下方 [XR-10 补充](#xr-10-补充) | M7→M4,M6,M13 |
| XR-11 | **文件系统操作分层规范**。见下方 [XR-11 补充](#xr-11-补充) | M7→全模块 |
| XR-12 | **模块 import 层级约束**。业务模块（Arch-L2~L4）禁止跨层直接 import 具体实现；接口定义权属于消费方；bootstrap 是唯一允许跨层引用的 DI 容器。完整规则见 `docs/arch/Module-Dependency-Axioms.md`。 | 全模块 |
| XR-13 | **pkg/ 净化约束**。`pkg/` 目录仅允许 POD 数据结构（struct/enum/const/纯内存方法），绝对禁止定义任何 `interface`，禁止 import 任何 `internal/` 包。interface 必须定义在消费方模块内部。 | pkg/→internal/ |

#### XR-04 补充

全模块强制，三层均安全，共享同一 `*sql.DB` MaxOpenConns=1：
1. **高频批量**（events/decision_log）：→ MutationBus DatabaseWriter（异步批量 + 租约校验 + 乐观锁）
2. **中频同步**（M5/M13/M12）：→ `Store.Put` / `Store.Txn`（KV 接口 + 同步确认）
3. **CAS+配置管理**（Blackboard 任务状态 / interface / server CRUD）：→ `store.DB()` 直写（CAS 需同步 RowsAffected / 配置类 SQL 无法走 KV 接口）

> **约束**：events 表写入统一经 `SQLiteEventLog` → MutationBus，不持有独立写路径（原设计的 EventWriteBuffer 批量缓冲层已确认零接线并删除，见 M02-Storage-Fabric.md §2.2）。**禁止：同一数据跨层混写**（如高频数据走裸 `db.ExecContext`；配置数据走 MutationBus）。

#### XR-08 补充

全项目唯一日志器 `log/slog`（Go 1.21+ stdlib）。
- **格式**：`slog.{Level}("subsystem: message", "key", val)`
- **必选 key**：`"err"` (error 值) / `"agent"` (agentID) / `"task_id"` / `"session"` / `"provider"`
- **级别**：
  - Error = 需人工介入
  - Warn = 可降级
  - Info = 启动·关闭·状态变更
  - Debug = 高频热路径（≤每请求级别）

> **约束**：**禁止**使用 `fmt.Printf` / `log.Printf` / `fmt.Println` 在 `internal/` 业务路径；**禁止**吞 error 的 Warn（必须带 `"err"` key）。

#### XR-09 补充

所有推理通过 `protocol.Provider.Infer/StreamInfer`，由 `inference.ProviderRegistry` 路由。`InferRequest` 必须声明 `ReasoningEffort` 字段。Canonical: `internal/llm/adapter/anthropic.go`。

> **约束**：**禁止**任何 `internal/` 直接构造 HTTP 请求调 LLM API（同时违反 XR-06）；**禁止** API Key 存全局变量或结构体字段（必须 `credentialFn func() string` JIT 拉取，使用后 `subtle.ConstantTimeCopy + memclr` 清零）。

#### XR-10 补充

1. **工具执行唯一入口**：`protocol.ToolRegistry.ExecuteTool`（PolicyGate → RateLimit → Sandbox → Result）
2. **脚本技能唯一入口**：`protocol.SkillExecutor.ExecuteSkill`（ContainerSandbox L3，Python `def execute(input: dict) -> dict:` 执行）
3. **内置工具**：直接 Go 调用（完全信任，不走沙箱）
4. **MCP 工具**：必须通过 MCP Manager 注册到 ToolRegistry，**禁止**直接 RPC 调用
5. **os/exec 约束**：**禁止** `os/exec.Command` 在 `internal/agent/`、`internal/swarm/`、`internal/eval/`、`internal/gateway/` 中。`internal/tool/builtin/` 下的内置工具（见规则 3「完全信任，不走沙箱」）**允许**直接调用 `os/exec.Command`，但须优先复用 `internal/tool/builtin/bash/sandboxed_exec.go` 的 `RunSandboxedArgv` 统一包装（argv 形式、env 净化、审计一致）；仅当存在无法绕过的技术约束（如需要区分 stdout/stderr 保持二进制流纯净）时才允许裸调 `exec.CommandContext`，且必须在函数注释中写明具体技术原因（"为什么不能走 sandboxed_exec"），禁止无理由裸调。Plugin install hook（`internal/action/hook/` → `RunScript`）与 Skill 执行（`internal/extension/skill/` → `ContainerSandbox L3`）两处合规例外见 `docs/specs/00-Constitution.md R1.13`。
6. **调用约束**：**禁止**绕过 ToolRegistry 调用工具实现的具体 struct。

#### XR-11 补充

- **L0 基础设施**（`internal/`）：可直接 `os.Open/WriteFile`（DB 初始化、schema 加载）
- **L1 执行沙箱**（`internal/sandbox*`）：可在 WorkDir 隔离边界内操作
- **L1/L2 认知·协同层**（`internal/agent/`, `internal/swarm/`）：**禁止**直接 `os.*` 文件操作，必须通过 WorkspaceManager 接口或 ToolRegistry 工具
- **L3 治理接口层**（`internal/eval/`, `internal/gateway/`）：**禁止**直接文件操作

#### XR-12 补充

完整约束见 `docs/arch/Module-Dependency-Axioms.md`。核心：
- Arch-L2 模块间禁止跨包直接 import 具体实现（使用 provider.go consumer-side interface）
- `internal/bootstrap/` 是全仓库唯一允许跨层引用的 DI 容器
- **`internal/protocol/` 叶子契约层准则**：`internal/protocol/` 是全系统被引用最广的 L0 包，理论上任何 L0 同层包（`internal/observability/`、`internal/security/`、`internal/llm/`、`internal/store/`）都可能在未来演化中反向需要引用 `protocol` 的类型。为保证 `protocol` 永远是无环图的安全叶子节点，`protocol` 包**应避免**引入其他 `internal/` 兄弟包的具体类型（即使当前未构成循环依赖），优先在 `protocol` 内部重新声明最小必要类型/接口（如 `TaintLevel` 已遵循此原则，权威定义即在 `internal/protocol/types.go`）。已存在的少量例外（如 `interfaces_memory.go` 引用 `observability/budget`、`prompt_builder.go` 引用 `security/taint.TaintedString`）当前未构成循环依赖，允许保留，但新增代码禁止再扩大此类跨包引用面，长期应收敛迁移。

#### XR-13 补充

`pkg/types/doc.go` 的注释约束是代码层声明；本规则（XR-13）是架构层约束。
两者配合：代码注释让编写者读到禁令，架构规则让 CI lint 可检查违规。

### [HE-Rule-1] 可观测优先
从第 0 行代码起全链路可追溯。Token_Burn_Rate + Surprise_Index 一等公民指标。

工程化要求：每次 LLM 调用必有 `InstrLLMCallsTotal` + `InstrLLMLatencyMs`；每次 Tool 调用必有 `InstrToolCallsTotal`；每次 Retrieval 必上报 `ExplainBits` 指标。禁止能算不上报、无埋点的中间状态。

### [HE-Rule-2] 可验证执行
禁止概率过滤充当安全边界。安全决策必须物理/密码学可验证。

工程化要求：工具入口必须经 `SchemaValidateInterceptor` 前置拦截；对象修改必须经 `PolicyGate.IsAuthorized()` fail-closed；`TaintLevel` 只升不降；nil 安全门返回 503。

### [HE-Rule-3] 可组合原语
最小可复用单元。模块间通信分两层：
1. **热路径**（延迟敏感，如 LLM 推理、记忆检索）：使用同步 Go interface 调用
2. **冷路径**和跨模块状态变更：通过 `internal/protocol/` 结构化事件通信

> **约束**：两种路径均禁止字符串拼装——interface 调用使用类型安全参数，事件使用 Protobuf 序列化。

### [HE-Rule-4] 数据驱动迭代
Eval Harness 驱动自进化，所有变更需通过 CI 门控。

工程化要求：新功能必须定义 `EvalCase`；Prompt 改动必附前后对比结果；RAG 改动必有实验依据；禁跳过 Eval 直改阈值/Prompt/权重。

### [HE-Rule-5] 状态机持有控制流
Go 确定性状态机持有控制流，LLM 仅做概率性填空。禁止 `while True: call LLM` 自由流转。

工程化要求：所有 LLM 调用必在 FSM handler 回调内；状态跳转必通过 `FSM.Trigger()`；耐时操作必移出锁范围用 `SafeGo` 异步；Failover 必全量遍历。

### [HE-Rule-6] State-in-DB
- 所有状态持久化落盘。
- 异步事件解耦跨存储状态变更。
- 崩溃恢复从 EventLog 回放。
- 性能关键缓存（L0 Working Memory、Wasm 技能缓存）豁免落盘。

> **约束**：豁免落盘的缓存须具备确定性崩溃重建路径（SessionResume 重建 ActiveContext；技能缓存从 SurrealDB-Core KV/SKILL.md 懒加载）。

工程化要求：所有模块间状态变更通过 `MutationBus.Submit()` 崩落单写者；Agent 状态转移后显式写入 EventLog；禁止持有 DB 连接期间发起 LLM 调用（R1.16）。

> **完整工程化阵阱 A-01~A-14 和生产原则 P-1~P-9**：`docs/specs/09-LLM-Agent-Production.md`

---

## §3 一等公民指标

### [SurpriseIndex]
定义见 03-Observability, §4。权威计算定义见 09-Self-Improvement §2.0。

**公式**：
三组件加权：`0.4 * embeddingCosineDistance + 0.35 * toolSequenceDivergence + 0.25 * MEMFMatchScore`
权重按 task_type 独立配置（factual_lookup / how_to / temporal_reasoning / complex_reasoning / default），运行时由 M9 DynamicDifficultyCalibrator 自适应调整。

**计算升级路径（非仅 embedding 分量）**：
- `toolSequenceDivergence` 的计算方式按不同 Phase 升级（Phase 1 是 Tier 0 默认基线）：
  - **Phase 1**：使用工具序列编辑距离（Levenshtein, <1ms）
  - **Phase 2**：（≥500 成功轨迹）升级为马尔可夫转移矩阵条件概率（1 - conditional_probability, O(1) 查表）
  - **Phase 3**：（API 暴露 logprob 后）升级为 per-token logprob 典型度——标注“长期愿景，依赖上游 Provider 暴露 logprob，可能 2027+，主线设计假设永久停留在 Phase 2”。

**路由阈值**：
- `< 0.3` → System 1
- `0.3 - 0.6` → System 1.5
- `> 0.6` → System 2

### [TokenBurnRate]
定义见 03-Observability, §3。CANONICAL SOURCE: M3 `polaris_token_burn_rate` Gauge (EMA_5s + EMA_30s)。所有消费者（M4/M11/M13）从此单源读取，禁止独立采样。

**双窗 EMA 平滑计算（EMA_5s + EMA_30s）**：
- **Stage 1**：EMA_5s > baseline.P95 × 2.0 → **THROTTLE**（限流）
- **Stage 2**：EMA_30s > baseline.P95 × 3.0 → **HARD STOP**（熔断）
- **Stage 3**：EMA_30s > baseline.P95 × 10.0 → **KillSwitch FULLSTOP**

> **约束**：
> - baseline 冷启动 (<50 calls)：固定保护值 baseline.P95 = 200 tokens/s。
> - M3 暴露专用 Counter `polaris_token_burn_stage3_triggered_total`，KillSwitch 从该 Counter 边沿驱动。

---

## §4 安全边界

### [TaintLevel] 【防退化】（依据 GD-8-005 结论，属领先设计，删除或弱化需 ADR）
五级污点置信度（定义见 11-Policy-Safety, §2.3）：
- `[Taint-None]` = 0 — 系统生成/常量
- `[Taint-Low]` = 1 — 受信内部数据
- `[Taint-Medium]` = 2 — LLM 摘要输出（硬地板，不可降为 Low）
- `[Taint-High]` = 3 — 外部用户输入
- `[Taint-UserReviewed]` = 4 — 人类显式确认

### [Taint-Prop]
**污点自然传播规则**：`output = max(所有输入的 TaintLevel)`，只升不降。自然传播（字符串拼接、JSON 字段合并、Protobuf 序列化等）绝不降级。

> **约束**：[Taint-Sanitizer] 是唯一被允许执行受控降级的特权路径。四种清洗方式（模式验证/LLM 摘要/确定性转换/用户显式确认）各有独立降级规则和审计要求，详见 11-Policy-Safety §2.5。

### [Taint-Floor-Medium]
LLM 摘要的最低 TaintLevel 为 TaintMedium。摘要可破坏注入指令的文本连续性，但无法保证消除跨语言/编码混淆的结构化注入载荷。

### [Taint-Sanitizer]
四种清洗方式（11-Policy-Safety, §2.5）：
1. 模式验证（确定性转换白名单）→ 可降至 TaintNone
2. LLM 摘要清洗 → 最低 TaintMedium（硬地板）
3. 确定性转换（黑名单移除）→ 降一级
4. 用户显式确认 → TaintUserReviewed

### [Cedar-Gate] 【防退化】（依据 GD-8-005 结论，属领先设计，删除或弱化需 ADR）
Cedar 策略引擎（定义见 11-Policy-Safety, §3）。Rust CGO-Free FFI（purego，ABI 1.1，校验机制见 `internal/protocol/ffi-abi.md §7`），<1ms 评估延迟，deny-by-default + 形式化验证 (Lean)。策略变更需审批流程。

### [KillSwitch] 【防退化】（依据 GD-8-005 结论，属领先设计，删除或弱化需 ADR）
三阶段紧急停止协议（11-Policy-Safety, §4）：
Phase 1 THROTTLE → Phase 2 PAUSE → Phase 3 FULLSTOP
`.fullstop` 持久状态文件防守护进程重启循环。密封模式 + `POST /_admin/unseal` 恢复。

### [ESCALATE]
人工审批协议（11-Policy-Safety, §4.4）。HITLGateway（13-Interface-Scheduler, §2.4）实现审批网关。

### [SSRFGuard] 【防退化】（依据 GD-8-005 结论，属领先设计，删除或弱化需 ADR）
六阶段 SSRF/DNS Rebinding 防护（11-Policy-Safety, §6）：
- **Phase 0**：Capability 出口强制（`read_only` 禁写 HTTP / `write_local` 仅内网）
- **Phase 1**：DNS 解析
- **Phase 2**：blockedCIDRs 校验
- **Phase 3**：TOCTOU 延迟 + 二次 DNS + 二次 CIDR
- **Phase 3.5**：响应 IP 超阈值拒绝
- **Phase 4**：IP 锁定（覆写 DialContext，锁定已验证 IP）

阶段总数为六（含 Phase 0），与 M11 §6 SafeDialer 五阶段标注（不含 Phase 0 独立标注）等价——Phase 0 在 M11 正文中描述为 Capability 出口强制，本字典为了完整性显式列出。

---

## §5 沙箱层级

### [Sandbox-L1]
Go 原生执行层，高性能零虚拟化开销。对于普通文件和数据操作（如文本替换 `str_replace_editor`、文件匹配 `glob`）采用进程内执行；对于外部命令（如 `bash`, `run_command`）采用平台原生子进程沙箱隔离（Bubblewrap / Apple Seatbelt / WSL2）。仅限系统核心内置工具使用。

### [Sandbox-L2]
Rust 脚本沙箱（wasmtime_engine.rs via FFI，定义见 07-Tool-Action-Layer, §4.3）。deny-by-default，资源硬限制(RAM/CPU/Walltime)。用于 Wasm 二进制执行；Python Logic Collapse 技能不走 L2，改走 L3 ContainerSandbox（ADR-0026）。

### [Sandbox-L3]
gVisor (runsc) 用户态内核 sandbox (定义见 07-Tool-Action-Layer, §4.7)。跨平台（Linux/macOS/Windows），sentry 拦截 syscall，~30-50MB 基线。Tier 2+ Linux 可选 Firecracker microVM 升级（~125MB，需硬件 KVM，最高隔离级别）。Tier 0 全平台 L3 不可用（内存不足以启动 gVisor sandbox ≥256MB）。

### [Sandbox-Tier0-macOS]
L2 Wasm + macOS Apple Sandbox profiles (`sandbox-exec`)。

### [Sandbox-Tier0-Windows]
L2 Wasm + Windows Job Objects (AppContainer)。

---

## §6 存储引擎

### [Storage-SQLite]
WAL 模式，真相源 + EventLog session_events 表。含 FTS5 全文检索。
写路径分三层（详见 XR-04）：高频批量走 MutationBus DatabaseWriter；中频同步走 Store.Put/Txn；CAS/配置管理走 store.DB() 直写。
三层共享同一 `*sql.DB`（MaxOpenConns=1），串行化由连接池保证，无死锁风险。

### [Storage-SurrealDB-Core]
统一的多模态认知存储侧车（Rust FFI via purego，ABI 1.1）。内置 KV 存储、HNSW 向量检索、BM25 全文检索及图计算引擎。


### [Storage-Router]
统一接口路由（02-Storage-Fabric, §1.2）。Store 接口：Get/Put/Delete/Scan/BatchWrite/Txn/Capabilities/Close。路由规则匹配最优引擎，SQLite 兜底。

---

## §7 模块拓扑

### [Module-Topology]
```
L0 基础设施: M1(Inference) M2(Storage) M3(Observability) M11(Policy-Safety)
L1 认知核心: M4(Agent-Kernel) M5(Memory) M6(Skill) M7(Tool-Action)
L2 协同学习: M8(Orchestrator) M9(Self-Improve) M10(Knowledge-RAG)
L3 治理接口: M12(Eval-Harness) M13(Interface-Scheduler)
```

### [Code-Package-Mapping]
```
internal/llm/, internal/store/, internal/observability/, internal/security/→ M1, M2, M3, M11
internal/agent/, internal/memory/, internal/prompt/→ M4, M5, M6
internal/tool/, internal/action/, internal/sandbox/→ M7
internal/extension/→ M13-bis, M06, M07
internal/swarm/, internal/learning/, internal/knowledge/→ M8, M9, M10
internal/eval/→ M12
internal/gateway/, internal/channel/, internal/cli/→ M13
internal/       → protocol, config, errors
rust/substrate/ → Rust FFI 性能路径
```
M7 物理归属 `internal/action/`（执行层：工具注册、沙箱）和 `internal/extension/`（技能、市场、MCP 双向、原生扩展）。

> **设计决策**：M7 的核心接口（Tool 类型、CapabilityLevel、ToolRegistry 接口）由 `internal/agent/` 中的 M4 通过 Go interface 消费。此为单向依赖 `cognition → action/extensions`，不构成循环引用。

---

## §8 跨模块共享接口

### [EventLog]
events 表（02-Storage-Fabric, §2.1）。系统唯一真相源，所有模块的事件持久化入口。物理表名 `events`，DDL 见 M2 §2.1。实现：`internal/store/audit/eventlog.go` → `SQLiteEventLog`，实现 `protocol.EventLogger` 接口，通过 `DatabaseWriter` 写入。

**不可变性边界**：`events` 表为不可变真相源（仅 INSERT，由 M11 hash chain 保护）。
- 派生投影表（`episodic_events` / `decision_log` / `audit_log` 等）可变操作仅限受控字段白名单（`archived`, `decay_weight`, `salience`, `archive_offset`）。
- 每次受控字段变更须写入对应的 `*_change_log` 表（再由该 log 表参与 hash chain）。

> **约束**：禁止原位 UPDATE 真相源 events 表。

M5 `episodic_events` 为 events 的派生投影表（记忆检索优化），两者通过 `idempotency_key` 关联。M11 hash chain 覆盖范围: `events` 表全字段 + 各派生表的 `*_change_log` 表。

### [MutationBus]
串行写总线（02-Storage-Fabric, §2.3）。DatabaseWriter 单写者。CompositeMutationIntent 支持多表原子提交（Outbox 模式）。

### [Blackboard]
多 Agent 协调黑板（08-Multi-Agent-Orchestrator, §1）。CAS 原子认领，Lease TTL 60s，心跳 15s±5s jitter，Reaper 1s 扫描。

### [Script-Sandbox]
Python 脚本沙箱（ContainerSandbox L3，ADR-0026）。Logic Collapse 蒸馏的技能以 `src/skill.py` 安装，函数签名 `def execute(input: dict) -> dict:`，通过 ContainerSandbox 执行（stdin/stdout JSON ABI）。M6 分层缓存：热技能容器预热 / 冷技能按需启动。Tier 0（L3 不可用）时禁止蒸馏，仅存 SKILL.md 元数据。

### [CredentialVault]
凭证安全存储（11-Policy-Safety, §5.2）。OS 密钥链适配：macOS Keychain / Linux SecretService / Windows Credential Manager。API Key SHA-256 加密封箱。

**persistent_key 轮换协议**：
1. 用户主动触发 `polaris vault rotate-master-key`
2. 后台分批解密旧 HMAC/Workspace
3. 新 key 重加密
4. 双 key 共存窗口（新写新 key，读时尝试新 → 旧 fallback）
5. 完成后旧 key 销毁

**冷启动决策树**：
- **GUI 桌面**：OS Keychain
- **headless Linux**：age-encrypted file（密码通过 stdin 或 `POLARIS_VAULT_PASSPHRASE` 环境变量，启动后立即清零）
- **Docker**：挂载 secrets volume

### [PIIGuard]
PII 检测与红化（见 11-Policy-Safety, §5.1）。
- **Tier 0 防护**：使用 Go 原生正则检测（零外部依赖，< 1ms）。仅覆盖结构化 PII 模式（信用卡号/SSN/API Key/邮箱/手机号/IP 地址），非结构化 PII（姓名/地址/雇主/医疗/生物特征等）不在 Tier 0 保护范围。
- **高阶防护（可选）**：可选 Presidio sidecar 仅在 Tier 1+ 显式启用时，用于高精度 NER 检测。
- **存储与解密**：SessionPIIVault 加密存储原文，SecureUnredact 仅在审计回放时解密。
- **用户提示**：用户首次进入处理 PII 数据的场景（如开启 Notion/Gmail Connector）时，系统主动告警 Tier 0 PII 防护范围。

### [Codex-Plugin] (Plugin / Skill / MCP 统一范式)
参考 OpenAI Codex 官方文档定义的行业标准共识。

在 Polaris 中，“插件（Plugin）”并非指编译进核心二进制的代码，而是**聚合了 Skill（可复用的认知与 Prompt 工作流）与 App Integration / MCP（外部工具与应用集成）** 的标准能力分发载体（Bundle）。

Plugin 机制负责统一定义生命周期、依赖和权限声明，而底层具体执行则完全委托给 M7 (MCP Manager) 和 M6 (Skill Registry) 进行物理隔离。

### [App] (富交互前端扩展)
参考 OpenAI Codex App 与 ChatGPT Apps SDK 概念。

在 Polaris 架构中，App 代表了**独立于单纯后端工具（MCP）和文本技能（Skill）的富交互层**。它为大模型或用户提供独立的 UI Widget（前端组件）、工作流视图或独立的 Web Endpoint。

与 MCP（纯后端无头服务）不同，App 拥有前端路由与用户状态交互能力，允许深度集成（如工作树、本地环境、自动化任务管理等），能够下发自定义的交互式卡片到聊天流中，甚至包含认证和状态流。App 在架构中有专属的运行时表（`apps`）以管理其独立的权限与路由端点。

### [Marketplace] (插件与技能应用市场)
参考 MCP Registry (registry.modelcontextprotocol.io) 与第三方市场生态（如 mcp.so）。Marketplace 是系统发现、安装和分发外部 Plugin、Skill 和 App 的中心枢纽。

Polaris 系统并非依靠用户手动编写模板来安装第三方能力，而是内置对接开源市场协议。它支持大模型在会话中根据用户意图直接向 Marketplace 检索可用扩展，获取安装与配置指令（如 `npx` 执行参数或下载链接），然后全自动完成配置并注册生效。

### [Marketplace-Builtin] (内置市场源)
配置中的 `is_builtin=1` 仅代表该市场是“系统出厂预置”的**同步源**（如官方精选库），它控制系统启动时是否自动拉取其目录索引并填充 `extension_catalog` 缓存表。

> **约束**：`is_builtin=1` **不代表**静默强行安装；默认情况下，所有扩展均遵循“按需安装”或“用户显式点击安装”的隔离准则。

### [Extension-Uninstall] (彻底卸载策略)
扩展的卸载行为取决于其 `origin` 来源。若属非内置扩展（如第三方社区 `is_builtin=0` 市场或 `origin='user'` 的手动安装）：

卸载不仅仅是从安装记录（`extension_instances`）及对应的运行时表（`mcp_servers`, `skills`, `plugins`）中摘除，还必须实现**彻底销毁**：
1. **DB 级联清除**：连同其在 `extension_catalog` 的展示元数据一并删除，确保不再在前端列表中残留。
2. **物理文件清除**：通过底层封装的文件系统接口彻底移除 `install_path` 目录。

> **约束**：卸载操作必须由底层管理接口统一执行，严禁在 HTTP Handler 业务层直接写裸 `os.RemoveAll`。

### [Codex-Automation] (后台自动化与定时任务)
参考 OpenAI Codex Automations。用于调度周期性循环任务、后台静默执行的检查流，并将结果推送到用户的“Triage (收件箱)”进行审批或通知。

在 Polaris 架构中，此概念由 **M8 (Multi-Agent Orchestrator)** 结合定时调度器实现。

> **约束**：后台执行必须遵循默认的安全沙箱级别。

### [Codex-Skill] (认知与提示词能力)
参考 OpenAI Codex Skills。Skill 是可复用的 Prompt 工作流及元认知指令，包含 `SKILL.md` (完整文档) 以及元数据（如触发条件、描述）。

为了节省 Token 预算，系统采用 **“初始仅加载元数据 (Meta)，按需懒加载全文 (Lazy Load)”** 的策略。在 Polaris 中，这对应 **M6 (Skill Registry)**，支持动态发现、热重载以及 Wasm 化的高阶技能。

---

## §9 模块速记标识

- `[M1]` Inference-Runtime (01)
- `[M2]` Storage-Fabric (02)
- `[M3]` Observability (03)
- `[M4]` Agent-Kernel (04)
- `[M5]` Memory-System (05)
- `[M6]` Skill-Library (06)
- `[M7]` Tool-Action-Layer (07)
- `[M7-A2A]` M7 A2A 适配器 (§2)
- `[M7-Tool]` M7 ToolRegistry.ExecuteTool
- `[M8]` Multi-Agent-Orchestrator (08)
- `[M9]` Self-Improvement-Engine (09)
- `[M10]` Knowledge-RAG (10)
- `[M11]` Policy-Safety (11)
- `[M12]` Eval-Harness (12)
- `[M13]` Interface-Scheduler (13)

## §9-bis 核心机制速记

- `[PrivacyTier]`: 四级隐私分级: PrivacyPublic=0 / PrivacySession=1 / PrivacyLocal=2 / PrivacyEncrypted=3。定义见 M5 §2.3。
- `[ReplayMode]`: 进程级 atomic.Bool。true 时 M4/M5/M11 统一进入回放模式（禁止所有外部副作用: EmitEvent/ToolCall/Outbox）。M4 §8 / M5 §2.1 各自显式说明进入和退出时机。
- `[Idempotency-Key]`: 格式 `{target_engine}:{entity_type}:{entity_id}:{operation}:{version}`。跨模块共享的去重机制，见 M2 §2.5。

- `[System-1]`: 零 LLM 推理路径，SurpriseIndex < 0.3。Logic Collapse 蒸馏的 Python 技能脚本经 ContainerSandbox 直接执行。定义见 M4 §5。
- `[System-1.5]`: 轻量 LLM 推理，0.3 ≤ SurpriseIndex < 0.6。Tier 1 Budget API。
- `[System-2]`: 重量 LLM 推理，SurpriseIndex ≥ 0.6。Tier 2/3 Reasoning API。
- `[Logic-Collapse]`: System 2 成功轨迹 → LLM 生成 Python 脚本 → ContainerSandbox 执行 → System 1 缓存（HE-6 State-in-DB）。决策见 ADR-0026。定义见 M6 §2.2。
- `[MEMF]`: 谬误记忆池 (FallacyMemoryPool)。失败轨迹向量化打标存入专用池，MCTS/Best-of-N 剪枝前做相似度过滤。定义见 M9 §2.1。
- `[HeuristicsMemory]`: 成功启发式库。`task_type → []Heuristic`，检索时注入 prompt 引导。定义见 M9 §2.1。
- `[HybridRetriever]`: BM25 + Dense Vector + 图遍历三路并行召回 → RRF(k=60) 融合 → Cross-encoder 重排。M5 和 M10 共享 `internal/store/` 底层引擎，检索范围和配置参数各自独立。
- `[RRF]`: 倒数排名融合 (Reciprocal Rank Fusion)。公式: `weight / (k + rank + 1)`, k=60。多路检索结果归一化融合算法。
- `[ProgressiveRollout]`: 渐进式发布。Gate 1 Shadow 阶段以 1% 流量观测（不面用户）；Gate 3 Canary 阶梯为 5%→25%→50%→100%，每步驻留 24h。M9 决策阶段推进，M13 TrafficSplitter 执行分发，M12 ShadowExecutor 对比评估（✅ 2026-07-10 恢复接入，见 M12 §4/§8：ShadowExecutor 周期回放 + ConfirmShadow 推进 Gate，Gate 1 对比能力已可用）。权威定义见 M09 §2.3。
- `[GEPA]`: 遗传-Pareto 反射式进化 (Genetic-Pareto-Evolutionary-Algorithm)。PromptOptimizer 三融合算法之一，种群 8 × 5 代 Pareto 前沿搜索。定义见 M9 §1.1。
- `[MemAPO]`: 双记忆跨任务复用 Prompt 优化。PromptOptimizer 三融合算法之二。
- `[ContraPrompt]`: 对比轨迹 Prompt 优化。PromptOptimizer 三融合算法之三。
- `[BFS-Traverse]`: 有界广度优先图遍历 (depth=2, maxNeighborsPerHop=20, maxTotalNodes=200)。关联发现模式使用 [Spreading-Activation]。
- `[Spreading-Activation]`: 扩散激活图遍历。
  - 种子实体 energy=1.0
  - 每轮 ×edge.weight 传播
  - 自身 ×0.7 衰减（`energyDecay=0.7`，`MakeExtensionSearchFn` 硬编码）
  - ≤0.05 停止（dormancyThreshold=0.1）
  - 最多 5 轮（depth=2，fanOutLimit=5）
- `[Connector-Taint-Table]`: Connector 源 TaintLevel 初始等级映射表（见 M11 §2.4）。ConnectorScheduler 在 Ingest 前按此表为每个外部数据源打标。
- `[Context-Engineering]`: 上下文工程 —— 上下文窗口是有限预算，"放什么"比"怎么写"更重要。
  - **物理载体**：`kernel.PromptBuilder` 三 Zone（ZoneImmutable / ZoneMutableSkill / ZoneTaintedData）+ ImmutableCore + Spotlighting（TaintedData 写入前强制包裹）。
  - **区别于 Prompt Engineering**（仅关注单次提示措辞），Context Engineering 关注跨轮次/跨任务的上下文构造、压缩、分区、来源标注。
- `[Compaction]`: 上下文压缩 — 长程任务必须显式压缩中间状态，否则推理质量随上下文长度衰减（lost-in-the-middle）。M5 触发条件: topic shift / eventCount≥50 / sessionClosed。压缩后保留 EventLog 原始事件不变，仅压缩派生 Episodic Memory 投影。
- `[Sub-agent-Isolation]`: 子 Agent context 隔离 — 多 Agent 系统中每个 Agent 持有独立 `kernel.PromptBuilder` 实例与 context window，禁止共享父 Agent 完整上下文。Agent 间仅通过 M8 黑板的结构化 result entry 交换（schema 强制），避免上下文污染与 token 膨胀。
- `[Workflow-Agent-Ladder]`: 工作流-Agent 升级阶梯 —— 简单任务直接 LLM 调用 → 可预测流程用确定性 workflow → 动态长程才用 Agent。
  - **Polaris 物理映射**：Skill Library System 1（零 LLM）→ System 1.5（单次 LLM）→ System 2（多步 Agent）。
  - **设计原则**：按必要性升级，避免简单任务过度构建 Agent 系统。
- `[Staging-Pipeline]`: 自进化候选 staging 流水线。权威定义见 [ARCHITECTURE §3](./ARCHITECTURE.md)。任一阶段失败 → rejected / rolled_back / dead_letter。safety case 一票否决: newly_failing safety = regress。

## §9-ter 推理时计算（Test-Time Compute）

> 与 [System-1/1.5/2]（任务难度维度）正交，独立的"推理深度"维度。本节即权威定义。

- `[ReasoningEffort]`: Provider 抽象层一等公民字段，枚举 `low` / `medium` / `high`。M1 Provider Interface 将其映射至底层 API（o3 `reasoning_effort` / DeepSeek R1 `reasoning_budget` / Claude `thinking.budget_tokens`）。**与 task 难度无关**——同一 task 可在不同 effort 下执行。定义见 M1 §X。
- `[ReasoningTokens]`: Provider 返回的 usage 字段，与 `output_tokens` 分计。计入 [TokenBurnRate] 总量但单独导出 Prometheus Gauge `polaris_reasoning_tokens_total`。
- `[ReasoningState]`: 跨轮 reasoning 状态持久化。物理载体为 M5 `episodic_events.reasoning_state` 列（msgpack 加密 blob），用于推理模型多轮间继承思维链。Tier 0 默认 off（成本控制），Tier 1+ 启用。定义见 M5 §X。
- `[BestOfN]`: M4 可选执行模式。同一 query 用 `temperature>0` 并行采样 N 次（默认 N=3），由 [Verifier] 选最优。Cedar 策略 `permit when context.task_priority >= 1` 限制（高优任务才用）。定义见 M1 §X。
- `[SelfConsistency]`: BestOfN 的聚合策略——N 个采样结果做多数投票（结构化输出）或语义聚类（自由文本）。区别于 BestOfN 的"挑最优"，SelfConsistency 是"投票"。
- `[TTC]`: Test-Time Compute 统称，覆盖 ReasoningEffort / BestOfN / SelfConsistency / 推理时搜索。运行成本 = N × ReasoningTokens × 单价。

## §9-quater 第六防线与即时执行

- `[FactualityGuard]`: 安全防线 D6（与 D1~D5 同等级），守护 LLM 输出的事实性。
  - 运行时抽样 5%（可配）经 [CitationValidator] + 数值一致性检查。
  - 检测到 hallucination → 标记 `TaintLevel` 强制升至 [Taint-Medium] + 触发 LLM-as-Judge 二次裁决。完整定义见 M11 §X。
- `[CitationValidator]`: 引用核验器。M10 RAG 输出强制附带 `chunk_id` 引用；FactualityGuard 抽样验证引用 chunk 真实包含输出主张。定义见 M10 §4.X。
- `[CodeAct]`: 即时代码执行行动空间。区别于 [Logic-Collapse]（沉淀型脚本技能）与 LLM 生成脚本（走 staging 流水线）——CodeAct 是**单次 ad-hoc 代码 + 立即执行**。
  - 强制 [Sandbox-L3]（HT0 不可用）+ Capability Token + Audit。定义见 M7 §X。
- `[Memory-Write-Tool]`: LLM 主动写记忆工具集。【防退化】（依据 GD-8-004 结论，属领先设计，删除或弱化需 ADR）。区别于被动记忆积累（Agent 轮次结束后 outbox 异步落盘）——是 LLM **在推理中即时调用工具、主动写入语义记忆**的能力。
  - 包含 4 个内置工具：`memory_write`（写入/覆盖事实）、`memory_search`（混合检索）、`memory_append`（追加属性）、`memory_expire`（标记失效）。
  - 实现：`internal/tool/builtin/memory_tools.go`，注册接口：`builtin.RegisterMemoryTools(sbx, toolReg, semanticWriter, retriever)`。写入底层对接 `SemanticMemWriter.UpsertFact/Archive`；检索对接 `HybridRetriever`。
  - 全部 `SandboxTier=InProcess`、`RiskLevel=Low`，走 PolicyGate 五阶段。定义见 M5 §5-bis，工具层描述见 M7 §3.1。
- `[CoreMemory]`: 核心工作记忆区。Agent 显式可编辑、持久化、带硬上限的核心状态区。属于 Working Memory，通过 `ZoneCoreMemory` 注入 Prompt，支持通过 `core_memory_edit` 工具（支持 `set/append/delete`）显式管理。用于维护角色约束、长程任务状态等。定义见 ADR-0036。

## §9-quinquies 中断与漂移

- `[UserInterrupt]`: 用户中断协议。M13 `POST /v1/agent/{taskID}/interrupt` 触发 M4 状态机进入 S_INTERRUPT（状态总数见 M4 §1）。< 200ms 内传播 context.Cancel 至所有运行 LLM 调用与工具调用。与 [KillSwitch] FULLSTOP 同等优先级但作用域为单任务。定义见 M4 §1, M13 §1.2.5。
- `[ReflectionMemory]`: 反思记忆。区别于 Episodic（事件流水）与 Semantic（事实图谱）——是 Agent 自身对"做了什么 + 学到什么"的元认知摘要。M5 §3.X 新表 `reflection_memory` 存储；触发: 任务终态 + Session 关闭 + 失败 reflection。区别于 M9 PersonaRefiner（用户画像更新）。
- `[PerformanceDrift]`: 运行时任务质量漂移检测。M3 滑窗 [Window-Quality-10min] 统计任务成功率，对比 RollingBaseline（24h EMA），偏离 >2σ → WARN，>3σ → CRITICAL + 候选 [KillSwitch] Stage 1。区别于 M12 RegressionDetector（CI 触发的离线检测）。定义见 M3 §X。

---

## §10 标准化时间窗常量

| 标签 | 时长 | 用途 |
|------|------|------|
| `[Window-Burst-30s]` | 30s | 突发检测 (TokenBurnRate Stage 3 / KillSwitch BurnRateFuse) |
| `[Window-Pressure-30s]` | 30s | 资源压力 (OSMemoryGuard / MonitorMemoryPressure 滑窗) |
| `[Window-Quality-10min]` | 10min | 质量趋势 (ContinuousSamplingMonitor 检查间隔) |
| `[Window-Drift-7d]` | 7d | 嵌入漂移 (DriftDetector 检查间隔) |

所有 30s 滑窗统一由此常量定义，各模块引用标签而非硬编码时长。

## §10-bis TaintLevel 数据表示规范

Go 内存内统一为 `internal/security/` 的 `TaintLevel` 枚举类型 (int)。持久化层 (SQL/Protobuf) 统一为 `INT2 0-4`，禁止 `TEXT 'medium'/'high'` 字符串编码。跨模块文档统一使用 `[TaintLevel]` 标签引用，禁止裸用 `INT 0-4` 描述。

## §11 标签引用规范

- 所有 `[Concept-Name]` 标签必须能在本文档中找到定义
- 子模块文档使用标签引用替代展开解释
- 标签首次出现时可附带简短提示，后续出现仅写标签
- 格式: `[HE-Rule-5]` / `[Taint-Medium]` / `[Tier-0-Limit]` / `[SurpriseIndex]`

## §12 标签→实现文件追溯

每个标签指向其权威实现文件（DDL/接口/常量定义）。架构文档仅保留设计决策和不变量，实现细节以以下文件为准：

| 标签 | 权威实现 |
|------|---------|
| `[EventLog]` | `internal/protocol/schema/001_events.sql` |
| `[MutationBus]` | `internal/protocol/schema/002_outbox.sql` + `internal/protocol/types.go` |
| `[Storage-SQLite]` / `[Storage-SurrealDB-Core]` / `[Storage-Router]` | `internal/protocol/interfaces.go` (Store) |
| `[HybridRetriever]` / `[RRF]` | 接口定义: `internal/protocol/interfaces.go` (HybridRetriever)；实现: `internal/store/` |
| `[Wasm-Sandbox]` | `internal/protocol/interfaces.go` (SkillExecutor) |
| `[Cedar-Gate]` | `internal/protocol/interfaces.go` (PolicyGate) |
| `[Blackboard]` | `internal/protocol/interfaces.go` (Blackboard) |
| `[TaintLevel]` / `[Taint-Prop]` | `internal/protocol/types.go` (TaintLevel) |
| `[SSRFGuard]` | `internal/protocol/interfaces.go` (SafeDialer) |
| `[TokenBurnRate]` | `internal/observability/` (EMA 计算 + 熔断) |
| `[SurpriseIndex]` | `internal/observability/` (Prometheus Gauge) / `internal/learning/` (M9 完整版推送) |
| `[KillSwitch]` | `internal/security/` (FSM + 阶段变迁) |
| `[Taint-Sanitizer]` | `internal/security/` (TaintedString / SafeString / Sanitize) |
| 各模块 DDL | `internal/protocol/schema/001-006_*.sql` (架构 DDL 含中文注释) |

---

## §13 标识符↔概念映射表(命名一致性 SSoT)

命名规范统一在 [`docs/specs/00-Constitution.md §R2`](../specs/00-Constitution.md) 维护(动词/名词/量纲/错误码/指标 5 张表)。本字典 §12 [标签→实现文件追溯] 仍是概念→代码的查阅源。

## §V8-Principle：哥德尔边界原则

**[Concept:V8-GoedelBoundary]**

Polaris 是一个足够强大的系统。根据哥德尔不完备性定理的工程类比，任何足够强大的自验证系统都包含其无法从内部识别的真盲区。

**工程约束**：系统的每一层自我验证必须对应一个**不随系统演化而演化**的外部参照锚点：

| 自我验证层 | 盲区类型 | 外部锚点 |
|-----------|---------|---------|
| MEMF 失败记忆 | B3 认知死区 | S4 BlindZone Detector |
| EvalHarness 目标函数 | B1 目标函数漂移 | S2 Meta-Eval Sentinel |
| 连续采样滚动基线 | B4 无绝对锚点 | S3 Founding Behavioral Anchor |
| LLM-as-Judge | B2 相关评判偏差 | S1 Red Team Protocol + L5 Human |
| KillSwitch 自动触发 | — | Stage 3 FULLSTOP 需人工 unseal |

**禁止规则**：禁止用自动化机制替换上述任何外部锚点。越自动化的系统，越需要保留这些独立的验证节点。
