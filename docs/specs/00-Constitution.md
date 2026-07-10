# 00 宪法规则（全局强制，不可违反）

> 所有 AI 生成的代码都必须满足以下规则。任何需求、截止日期、性能优化都不能成为违反的理由。

## R1 反模式目录（禁止清单）

| # | 反模式 | 表现 | 替代 |
|---|--------|------|------|
| R1.1 | 跨层越权调用 | service 调用 DAO、controller 含业务逻辑 | 调用只能逐层：ctrl → svc → dao |
| R1.2 | 裸 error 传播 | `return err` 不含上下文 | `return apperr.Wrap(apperr.CodeXxx, msg, err)` |
| R1.3 | 全局可变变量 | `var x = ...` 在 `internal/` 下（**豁免**：哨兵错误 `var ErrXxx = apperr.New(...)` 是 Go 惯例，`errors.Is` 依赖指针相等，不得改为 `apperr.New` 内联调用；ADR-0001 豁免 `observability/metrics` 一等公民指标；但可变状态单例如 `var GlobalXxx = NewXxx()` 仍违规） | 结构体字段 + 构造函数注入 |
| R1.4 | 接口定义在实现方 | dao 包暴露 `type Dao interface` | `internal/protocol/` 统一声明，`@consumer` 标记 |
| R1.5 | 超过 3 层嵌套回调 | if 套 for 套 select | 提取命名函数 + early return |
| R1.6 | 隐式字符串耦合 | 模块间通过字符串 topic/channel 通信 | `internal/protocol/` 结构化事件类型 |
| R1.7 | 违反层依赖方向 | L0→L1/L2/L3 任一引用 | 依赖单向 L0←L1←L2←L3，详见 `04-Module-Boundary.md B1` |
| R1.8 | comment 解释"是什么" | `// 创建用户` 在 `CreateUser` 函数上 | comment 只写"为什么"——设计意图、约束、坑 |
| R1.9 | LLM 自由流转 | `while true { call LLM }` 无状态机包裹 | Go FSM 持有控制流，LLM 是协处理器 |
| R1.10 | 概率过滤当安全边界 | `if rand > 0.5 { block }` 当安全措施 | 物理断裂：TaintTracking + sandbox + capability token |
| R1.11 | 绕过 Provider 直接调 LLM | `internal/` 内 `http.Post("api.openai.com/...")` 构造 LLM 请求 | `protocol.Provider.Infer/StreamInfer`（XR-09） |
| R1.12 | 业务路径直接打印 | `fmt.Printf` / `log.Printf` / `fmt.Println` 在 `internal/` 下 | `slog.Info/Warn/Error`（XR-08） |
| R1.13 | 绕过沙箱执行外部命令 | `os/exec.Command(...)` 在 `internal/agent/` `internal/swarm/` `internal/eval/` `internal/gateway/`（两处合规例外：① Plugin install hook 必须经 `internal/action/hook/` → `RunScript` 执行，不可裸调；② Skill 执行必须经 `internal/extension/skill/` → `ContainerSandbox L3`，不可裸调） | `protocol.ToolRegistry.ExecuteTool` + Sandbox（XR-10） |
| R1.14 | 安全门 nil 旁路 | `if installMgr != nil { /* 安全检查 */ }` 之后继续执行安装，nil 时相当于跳过策略层 | **安全门依赖必须 fail-closed**：nil → 返回 503，不得继续。调用方负责启动期注入断言 |
| R1.15 | MCP 子进程继承完整父环境 | `cmd.Env = os.Environ()` 不过滤，将宿主 `*_KEY/_TOKEN/_SECRET/_PASSWORD` 等密钥类环境变量传入 MCP stdio 子进程 | 统一调用 `sanitizeParentEnv()`，仅保留运行时必要变量并叠加显式配置的 `MCPClientConfig.Env` |
| R1.16 | 持有 DB 连接期间发起阻塞式外部调用 | `tx, _ := db.BeginTx(...); resp, _ := provider.Infer(...); tx.Commit()` 或 `rows, _ := db.QueryContext(...); for rows.Next() { provider.Infer(...) }` 未关闭 `Rows`/未提交 `Tx` 前调用 LLM 推理、网络请求等耗时不确定的外部调用，SQLite 连接池有限（`internal/store/store.go` writer `MaxOpenConns=1`）时会连带卡死其他读写请求 | 先读完数据 / `rows.Close()` / `tx.Commit()`（或 `Rollback()`）释放连接，再在无连接持有的上下文中发起外部调用；必要时拆分为"读事务 → 无锁调用外部 → 写事务" |

> **R1.4 例外澄清**（2026-07-10 补充，源于 Gemini 审核报告 GR-3-004 复核）：M7（`internal/tool/`、`internal/action/`）的核心接口（`Tool`/`CapabilityLevel`/`ToolRegistry`/`Catalog` 等）允许定义在实现方包内，前提是消费方为**明确的单向上游层**（如 M4 `internal/agent/` 通过 Go interface 消费，见 `docs/arch/00-Global-Dictionary.md §7`）。该例外**不适用于**没有任何真实消费方的接口声明——此类接口视为死代码，必须删除而非以"R1.4 例外"为由保留。新增 producer-side interface 前必须先 grep 确认消费方存在且位于允许的上游层，否则一律按 R1.4 反模式处理并归口至 `internal/protocol/`。

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
| 技能（Python 蒸馏，ContainerSandbox，ADR-0026） | `Skill` | Capability / Function |
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

格式：`Code<Category>`，权威源：`pkg/apperr/apperr.go`（唯一定义处，禁止在其他包新增 Code 常量）。

| 类别 | 现有值 |
|------|--------|
| 通用 | `CodeOK`, `CodeInternal`, `CodeInvalidInput`, `CodeUnimplemented` |
| 资源 | `CodeNotFound`, `CodeAlreadyExists`, `CodeConflict`, `CodeResourceExhausted` |
| 权限 | `CodeUnauthorized`, `CodeForbidden` |
| 超时/取消 | `CodeTimeout`, `CodeCancelled` |
| Provider/网络 | `CodeProviderExhausted`, `CodeNetworkUnavailable` |
| 污点 | `CodeTaintViolation` |
| 沙箱 | `CodeSandboxTier0Limit` |

AI 生成新错误码前必须 `grep -n "Code.*Code = " pkg/apperr/apperr.go` 检查同义码。重复定义视为 R1 反模式扩展。

### R2.6 指标命名

格式：`polaris_<subsystem>_<name>_<unit>`。

- subsystem 限定: `inference` / `storage` / `observability` / `policy` / `cognition` / `action` / `swarm` / `governance` / `edge`
- unit 限定: `total` / `count` / `bytes` / `seconds` / `ratio` / `rate`(无单位 Gauge 可省略)

示例: `polaris_inference_tokens_total`, `polaris_observability_token_burn_rate`, `polaris_storage_write_latency_seconds`。

## R3 HE-Rules 可验证工程量表

**Harness Engineering 六大不变量的工程化实现**。AI 生成每段代码后，必须逐条自查：

### R3-HE1 可观测优先（Observability First）

> 从第 0 行代码起全链路可追溯。Token_Burn_Rate + Surprise_Index 一等公民指标。

**自查问**：这条路径是否可追溯？有没有 TokenBurnRate / SurpriseIndex 的埋点？

**必须有**：
- 每次 LLM 调用：`InstrLLMCallsTotal.Add(...)` + `InstrLLMLatencyMs.Record(...)`
- 每次 Tool 调用：`InstrToolCallsTotal.Add(...)` + `InstrToolLatencyMs.Record(...)`
- 每次 Retrieval 返回：`ExplainBits` 属性填充 + `RecordExplainBit(ctx, "BM25"/"Vector"/...)` 上报
- 新开常驻的业务路径：必须在 `internal/observability/metrics/instruments.go` 注册对应指标

**禁止**：能算但不上报的中间状态、回落到无埋点的设计。

---

### R3-HE2 可验证执行（Verifiable Execution）

> 禁止概率过滤充当安全边界。安全决策必须物理/密码学可验证。

**自查问**：安全边界是物理断裂还是概率过滤？ Taint 是否显式传播？

**必须有**：
- 工具调用入口：`SchemaValidateInterceptor` 在 `Dispatcher.Use(...)` 管道首位注册
- 对象修改操作：必须经过 `PolicyGate.IsAuthorized()` 后方可执行（fail-closed）
- 结果内容：`TaintLevel` 属性 `只升不降`（ADR-0007 PropagateTaint）
- 安全门自身：`nil → 503 fail-closed`，禁止 `if gate != nil { 检查 } else { 放行 }`

**禁止**：
- `if rand() > 0.5 { block }` 类概率过滤当安全
- Taint 在中间环节被截断或被静默丢失
- `CodeInvalidInput` 错误不包装直接透传下游

---

### R3-HE3 可组合原语（Composable Primitives）

> 最小可复用单元。模块间通信分两层：热路径（延迟敏感）用 Go interface；冷路径和跨模块状态变更用结构化事件。

**自查问**：这个结构是直接依赖还是通过协议依赖？有没有循环依赖引入的风险？

**必须有**：
- 新接口定义在 `internal/protocol/`，不在实现方包
- 跨模块通信通过 `MutationBus` / `EventLog`，禁止主动连接调用对方包内改变函数
- 新工具注册通过 `protocol.ToolRegistry.ExecuteTool` 统一入口，不开后门旁路

**禁止**：`service 调用 DAO`，`controller 含业务逻辑`，字符串隐式耦合实现的文件名。

---

### R3-HE4 数据驱动迭代（Data-Driven Iteration）

> Eval Harness 驱动自进化，所有变更需通过 CI 门控。

**自查问**：这个变更是否有 Eval 可以验证？新加能力是否可评测？

**必须有**：
- 新功能定义 `EvalCase`（或复用已有），确保 `make test` 包含平行回归索引
- Prompt 改动：必须附带前后的 Eval 对比结果，禁止“感觉更好”开展 Prompt 改动
- RAG 改动：必须有切片策略、Embedding 选型、检索权重的实验依据，禁止拍脑袋硬编码

**禁止**：跳过 Eval 直接修改阈值 / Prompt / 权重，“看起来更好”不算上线条件。

---

### R3-HE5 状态机持有控制流（FSM Owns Control Flow）

> Go 确定性状态机持有控制流，LLM 仅做概率性填空。禁止 `while True: call LLM` 自由流转。

**自查问**： LLM 调用是否被状态机包裹？是否有无状态机保护的 LLM 自由流转？

**必须有**：
- LLM 调用出现在 FSM 某个状态的 `handler` 回调内，不存在裸露的 `callLLMAndWait`
- 状态跳转必须通过 `FSM.Trigger()` 发起，禁止直接赋值状态字段
- 耗时操作（扩展激活、反射写回）必须移出锁范围，用 `SafeGo` 异步执行
- 并发调用多 Provider 时：必须全量遍历可用列表直到耗尽，不得单次降级放弃

**禁止**：
- `while true { callLLM; decide; retry }` 无 FSM 包裹
- LLM 返回内容直接驱动 `goto`/`if-else` 巨大分支树，而非导出结构化模型后再进入 FSM
- FSM Handler 内裸起 goroutine 运行投 LLM 然后回写共享字段（未加锁竞态）

---

### R3-HE6 状态落盘（State-in-DB）

> 所有状态持久化落盘。崩溃恢复从 EventLog 回放。

**自查问**：状态是否落盘？崩溃后能恢复吗？ EventLog 是否追加了新版真相？

**必须有**：
- 所有模块间状态变更通过 `MutationBus.Submit()` 汇入单写者
- Agent 状态转移后显式写入 EventLog（`runFSM` 内在每次转移后触发）
- 崩溃恢复路径：必须从 `events` 表 / `outbox` 表可重建当前执行状态

**禁止**：
- 状态仅在内存（goroutine 局部变量）中存在，进程崩溃后丢失
- 用独立 checkpoint 文件替代 EventLog（参见 ADR-0006）
- 接受 DB 连接期间发起 LLM 调用（R1.16，事务卡死 + 超时双败）

---

> **扩展层**：Agent 工程化具体陷阱（A-01~A-14）和生产原则（P-1~P-9）详见 `docs/specs/09-LLM-Agent-Production.md`。

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

- `internal/` 各包按四层依赖方向单向引用（L0←L1←L2←L3），禁止反向引用
- `pkg/`（apperr/types/version）无业务逻辑，可被任意层引用
- 跨模块通道与 FFI 协议见 `04-Module-Boundary.md B1 / B2 / B4 / B5`

## R7 可读性硬上限

| 维度 | 上限 | 处置 |
|------|------|------|
| 函数体行数 | ≤ 60 | 超出必须拆分，除非 ADR 豁免 |
| 文件行数 | ≤ 400 | 超出必须按职责拆文件 |
| 嵌套深度 | ≤ 3 | 超出用 early return / 提取命名函数 |
| 圈复杂度 (gocyclo) | ≤ 15 | 超出拆分 + 表驱动 |
| 单函数参数数 | ≤ 5 | 超出用 struct 参数包 |
| 包内文件数 | ≤ 20 | 超出考虑拆子包 |

`.golangci.yml` 用 `gocyclo` / `nestif` / `lll` 机械化检查；CI fail-closed。
`funlen`（函数物理行数/语句数）不启用：与 `gocyclo` 高度冗余，且 Go 错误处理惯例天然拉长物理行数但不代表真实复杂，误报率偏高。复杂度治理职责收敛到 `gocyclo` 一家（判断依据见 `docs/arch/decisions/ADR-0013-lint-machinery-phase1.md` 修订记录 2026-07-04）。

> 为什么硬上限：AI 生成长函数无内省压力、不主动拆分。量化卡死是防 AI 输出膨胀的最小代价。

## R8 参考实现强制引用

写任何新代码前，必须先 Read `07-Reference-Implementation.md` 中对应模块的标杆文件。

- PR 描述必填字段：`参考实现: internal/xxx/yyy.go | 对齐 / 偏离原因`
- 偏离协议见 `07-Reference-Implementation.md §7.3`
- 单 PR diff ≤ 300 行；超出须拆分（见 `05-Coding-Workflow.md W6`）

> 为什么强制：AI 在无锚点时每次现编风格，导致同一模块三种实现并存。标杆是"消除局部连贯/全局混乱"最直接的物理对齐手段。
