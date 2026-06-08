# 00 宪法规则（全局强制，不可违反）

> 所有 AI 生成的代码都必须满足以下规则。任何需求、截止日期、性能优化都不能成为违反的理由。

## R1 反模式目录（禁止清单）

| # | 反模式 | 表现 | 替代 |
|---|--------|------|------|
| R1.1 | 跨层越权调用 | service 调用 DAO、controller 含业务逻辑 | 调用只能逐层：ctrl → svc → dao |
| R1.2 | 裸 error 传播 | `return err` 不含上下文 | `return perrors.Wrap(CodeXxx, msg, err)` |
| R1.3 | 全局可变变量 | `var x = ...` 在 `pkg/` 下 | 结构体字段 + 构造函数注入 |
| R1.4 | 接口定义在实现方 | dao 包暴露 `type Dao interface` | `internal/protocol/` 统一声明，`@consumer` 标记 |
| R1.5 | 超过 3 层嵌套回调 | if 套 for 套 select | 提取命名函数 + early return |
| R1.6 | 隐式字符串耦合 | 模块间通过字符串 topic/channel 通信 | `internal/protocol/` 结构化事件类型 |
| R1.7 | 违反层依赖方向 | L0→L1/L2/L3 任一引用 | 依赖单向 L0←L1←L2←L3，详见 `04-Module-Boundary.md B1` |
| R1.8 | comment 解释"是什么" | `// 创建用户` 在 `CreateUser` 函数上 | comment 只写"为什么"——设计意图、约束、坑 |
| R1.9 | LLM 自由流转 | `while true { call LLM }` 无状态机包裹 | Go FSM 持有控制流，LLM 是协处理器 |
| R1.10 | 概率过滤当安全边界 | `if rand > 0.5 { block }` 当安全措施 | 物理断裂：TaintTracking + sandbox + capability token |
| R1.11 | 绕过 Provider 直接调 LLM | pkg/ 内 `http.Post("api.openai.com/...")` 构造 LLM 请求 | `protocol.Provider.Infer/StreamInfer`（XR-09） |
| R1.12 | 业务路径直接打印 | `fmt.Printf` / `log.Printf` / `fmt.Println` 在 pkg/ 下 | `slog.Info/Warn/Error`（XR-08） |
| R1.13 | 绕过沙箱执行外部命令 | `os/exec.Command(...)` 在 pkg/cognition/ pkg/swarm/ pkg/governance/ pkg/edge/ **及 pkg/gateway/server/**（install hook 唯一例外：必须经 `ContainerSandbox.RunScript`，不可裸调） | `protocol.ToolRegistry.ExecuteTool` + Sandbox（XR-10） |
| R1.14 | 安全门 nil 旁路 | `if installMgr != nil { /* 安全检查 */ }` 之后继续执行安装，nil 时相当于跳过策略层 | **安全门依赖必须 fail-closed**：nil → 返回 503，不得继续。调用方负责启动期注入断言 |
| R1.15 | MCP 子进程继承完整父环境 | `cmd.Env = os.Environ()` 不过滤，将宿主 `*_KEY/_TOKEN/_SECRET/_PASSWORD` 等密钥类环境变量传入 MCP stdio 子进程 | 统一调用 `sanitizeParentEnv()`，仅保留运行时必要变量并叠加显式配置的 `MCPClientConfig.Env` |

## R2 命名规范字典

> 同一概念全仓库唯一词根。违反视为 R1 反模式扩展。同一文件中常量用 `const ( ... )` 块分组，不加 `=` 对齐空格。

### R2.1 结构骨架

| 层/类型 | 模式 | 示例 |
|---------|------|------|
| Package | 单数小写，简短 | `storage`, `inference`, `policy` |
| Interface | consumer-side, 动词+er | `Store`, `Provider`, `EventLogger` |
| Struct | PascalCase | `MemoryEntry`, `SQLiteBlackboard`, `ResourceGovernor` |
| 构造函数 | `NewXxx(deps)` | `NewOrchestrator(bb, registry, maxAgents)` |
| 公开方法 | 动词短语 | `FindByID`, `Save`, `ListenLoop`, `DispatchPending` |
| 私有方法 | camelCase | `dispatchPending`, `skillGapAnalysis` |
| 常量 | PascalCase 按类型分组 | `LayerWorking`, `TaskPending`, `TaintHigh` |
| Test func | `TestXxx_Scenario` | `TestStore_InsertDuplicate`, `TestOrchestrator_Timeout` |
| 文件名 | snake_case 包裹中横线 | `factuality_guard.go`, `surreal_store.go` |
| 测试文件 | `_test.go` 与被测同级同目录 | `factuality_guard_test.go` |
| Git 提交 | `<type>(<scope>): <简体中文>` | `feat(storage): 实现 SurrealDB-Core HNSW` |

### R2.2 动词词根(避免 `GetSkill` / `FetchSkill` / `LoadSkill` 并存)

| 语义 | 词根 | 反例（禁用） |
|------|------|------|
| 读单条 | `Get` | Fetch / Load / Retrieve |
| 读多条 | `List` | Query / Find（按条件查询用 `FindBy`） |
| 按条件读 | `FindBy<Field>` | Search / Lookup |
| 写新 | `Create` 或 `Insert` | Add / New（`New` 仅用于构造函数 `NewXxx`） |
| 改 | `Update` | Modify / Edit / Patch |
| 删 | `Delete` | Remove / Drop（`Drop` 仅用于 schema） |
| 持久化 | `Save` | Persist / Store（`Store` 是名词，存储引擎类型） |
| 触发动作 | `Dispatch` / `Trigger` | Fire / Emit（`Emit` 专用于 events 写入） |
| 写事件 | `EmitEvent` | LogEvent / WriteEvent |
| 校验 | `Validate` | Check / Verify（`Verify` 专用于密码学） |
| 评估 | `Evaluate` | Assess / Score |
| 注册 | `Register` | Add / Bind |

### R2.3 名词词根

| 概念 | 词根 | 反例（禁用） |
|------|------|------|
| 任务 | `Task` | Job / Work / Action（`Action` 是工具调用专用） |
| 计划 | `Plan` | Workflow / Pipeline（`Pipeline` 仅指 `Staging-Pipeline`） |
| 技能（Wasm 化） | `Skill` | Capability / Function |
| 工具（执行边） | `Tool` | Action / Operation |
| 记忆 | `Memory` | Cache / Store（`Store` 是存储引擎） |
| 黑板 | `Blackboard` | SharedState / Bus |
| 智能体 | `Agent` | Worker / Actor |
| 凭证 | `Credential` | Secret / Token（`Token` 仅指 `Capability Token` / `TokenBurnRate`） |
| 提供方 | `Provider` | Vendor / Service |
| 注册表 | `Registry` | Manager / Catalog |
| 路由器 | `Router` | Dispatcher / Switch |
| 守卫 | `Guard` | Validator / Filter |
| 网关 | `Gateway` | Bridge / Proxy（`Proxy` 仅指网络代理） |
| 配置 | `Config` | Settings / Options（`Options` 仅用于函数选项模式） |

### R2.4 量纲后缀

| 量纲 | 命名规则 | 示例 |
|------|---------|------|
| 时长 | `time.Duration` 类型，不带后缀 | `timeout`, `interval` |
| 时间戳 | `At` 或 `Time` 后缀 | `createdAt`, `expireTime` |
| 大小（字节） | `Bytes` 后缀 | `payloadBytes` |
| 速率 | `Rate` 后缀 | `TokenBurnRate` |
| 计数 | `Count` 后缀或复数 | `errorCount`, `events` |
| 阈值 | `Threshold` 后缀 | `surpriseThreshold` |
| 分数 / 比率 | `Score` 或 `Ratio` 后缀 | `confidenceScore` |
| 限额 | `Limit` 或 `Max<X>` | `MaxAgents`, `requestLimit` |

### R2.5 错误码命名

格式：`Code<Subsystem><Reason>`，权威源：`internal/errors/codes.go`。

| 类别 | 前缀 | 示例 |
|------|------|------|
| 通用 | `Code` | `CodeInternal`, `CodeNotFound` |
| 权限 | `CodePolicy` | `CodePolicyDenied`, `CodePolicyEscalate` |
| 资源 | `CodeResource` | `CodeResourceExhausted` |
| 污点 | `CodeTaint` | `CodeTaintHigh`, `CodeTaintSanitizeFailed` |
| 状态机 | `CodeState` | `CodeStateInvalidTransition` |
| FFI | `CodeFFI` | `CodeFFIABIMismatch` |

AI 生成新错误码前必须 `grep -r "Code[A-Z]" internal/errors/` 检查同义码。重复定义视为 R1 反模式扩展。

### R2.6 指标命名

格式：`polaris_<subsystem>_<name>_<unit>`。

- subsystem 限定: `inference` / `storage` / `observability` / `policy` / `cognition` / `action` / `swarm` / `governance` / `edge`
- unit 限定: `total` / `count` / `bytes` / `seconds` / `ratio` / `rate`(无单位 Gauge 可省略)

示例: `polaris_inference_tokens_total`, `polaris_observability_token_burn_rate`, `polaris_storage_write_latency_seconds`。

## R3 HE-Rules 可验证工程量表

AI 生成每段代码后，必须逐条自查：

- **HE-Rule-1（可观测）**：这段路径是否可追溯？有没有 TokenBurnRate / SurpriseIndex 的埋点？
- **HE-Rule-2（可验证）**：安全边界是物理断裂还是概率过滤？Taint 是否显式传播？
- **HE-Rule-3（可组合）**：这个结构是直接依赖还是通过协议依赖？有没有循环依赖引入的风险？
- **HE-Rule-4（数据驱动）**：这个变更是否有 Eval 可以验证？新加能力是否可评测？
- **HE-Rule-5（控制流）**：LLM 调用是否被状态机包裹？是否有无状态机保护的 LLM 自由流转？
- **HE-Rule-6（落盘）**：状态是否落盘？崩溃后能恢复吗？EventLog 是否追加了新版真相？

## R4 Tier-0-Limit 兜底

所有新特性必须自问："这台机器的 RAM 只有 8GB，能跑吗？"

- 不能的场景必须在 FeatureGate 后，Tier0 关闭，Tier1+ 打开
- 不能把高资源消耗作为默认路径

## R5 注释规范

- 中文，短句
- 只写"为什么"——《设计意图、约束、陷阱、非显而易见的行为》
- 不写"是什么"——好命名已经表达了
- 代码注释样式：`// 中文短句`，文档注释用完整句子

## R6 模块引用纪律

- `internal/` 可被任何 `pkg/` 引用，`internal/` 之间不互引
- 跨模块通道与 FFI 协议见 `04-Module-Boundary.md B2 / B4 / B5`

## R7 可读性硬上限

| 维度 | 上限 | 处置 |
|------|------|------|
| 函数体行数 | ≤ 60 | 超出必须拆分，除非 ADR 豁免 |
| 文件行数 | ≤ 400 | 超出必须按职责拆文件 |
| 嵌套深度 | ≤ 3 | 超出用 early return / 提取命名函数 |
| 圈复杂度 (gocyclo) | ≤ 15 | 超出拆分 + 表驱动 |
| 单函数参数数 | ≤ 5 | 超出用 struct 参数包 |
| 包内文件数 | ≤ 20 | 超出考虑拆子包 |

`.golangci.yml` 用 `funlen` / `gocyclo` / `nestif` / `lll` 机械化检查；CI fail-closed。

> 为什么硬上限：AI 生成长函数无内省压力、不主动拆分。量化卡死是防 AI 输出膨胀的最小代价。

## R8 参考实现强制引用

写任何新代码前，必须先 Read `07-Reference-Implementation.md` 中对应 `pkg/` 的标杆文件。

- PR 描述必填字段：`参考实现: pkg/xxx/yyy.go | 对齐 / 偏离原因`
- 偏离协议见 `07-Reference-Implementation.md §7.3`
- 单 PR diff ≤ 300 行；超出须拆分（见 `05-Coding-Workflow.md W6`）

> 为什么强制：AI 在无锚点时每次现编风格，导致同一 `pkg/` 三种实现并存。标杆是"消除局部连贯/全局混乱"最直接的物理对齐手段。
