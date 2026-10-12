# 00 宪法规则（全局强制，不可违反）

> 所有 AI 生成的代码都必须满足以下规则。需求、截止日期、性能优化都不能成为违反的理由。

## R1 反模式目录（禁止清单）

| # | 反模式 | 表现 | 替代 / 门控 |
|---|--------|------|------|
| R1.1 | 跨层越权调用 | service 调用 DAO、controller 含业务逻辑 | 逐层调用 ctrl → svc → dao |
| R1.2 | 裸 error 传播 | `return err` 不含上下文 | `apperr.Wrap(apperr.CodeXxx, msg, err)`。门控：`Test_inv_BareErrorReturnRatchet`、`make apperr-semantics-check` |
| R1.3 | 全局可变变量 | `var x = ...` 在 `internal/` 下。**豁免**：哨兵错误 `var ErrXxx = apperr.New(...)`（`errors.Is` 依赖指针相等，不得内联）；ADR-0001 的 `observability/metrics` 一等公民指标。`var GlobalXxx = NewXxx()` 式可变单例仍违规 | 结构体字段 + 构造函数注入。门控：`Test_inv_NoGlobalVar` |
| R1.4 | 接口定义在实现方 | dao 包暴露 `type Dao interface` | `internal/protocol/` 统一声明，`@consumer` 标记（例外见下） |
| R1.5 | 超过 3 层嵌套 | if 套 for 套 select | 命名函数 + early return |
| R1.6 | 隐式字符串耦合 | 模块间用字符串 topic/channel 通信 | `internal/protocol/` 结构化事件类型 |
| R1.7 | 违反层依赖方向 | L0→L1/L2/L3 任一引用 | 单向 L0←L1←L2←L3（`04 B1`）。门控：`Test_inv_NoCrossLayerImport` |
| R1.8 | comment 解释"是什么" | `// 创建用户` 标在 `CreateUser` 上 | 只写"为什么"：设计意图、约束、坑 |
| R1.9 | LLM 自由流转 | `while true { call LLM }` 无状态机包裹 | Go FSM 持控制流，LLM 是协处理器。门控：`make fsm-io-check`（Effects 内 IO、goto） |
| R1.10 | 概率过滤当安全边界 | `if rand > 0.5 { block }` | 物理断裂：Taint + sandbox + capability token |
| R1.11 | 绕过 Provider 直调 LLM | `internal/` 内 `http.Post("api.openai.com/...")` | `protocol.Provider.Infer/StreamInfer`（XR-09） |
| R1.12 | 业务路径直接打印 | `fmt.Printf` / `log.Printf` / `fmt.Println` 在 `internal/` | `slog.Info/Warn/Error`（XR-08）。门控：`Test_inv_NoBareLogPrint` |
| R1.13 | 绕过沙箱执行外部命令 | `os/exec` 在 `internal/agent|swarm|eval|gateway/`。合规例外仅两处：Plugin install hook 经 `internal/action/hook/` → `RunScript`；Skill 经 `internal/extension/skill/` → ContainerSandbox L3 | `protocol.ToolRegistry.ExecuteTool` + Sandbox（XR-10）。门控：`Test_inv_NoOsExecInHigherLayers` |
| R1.14 | 安全门 nil 旁路 | `if installMgr != nil { 安全检查 }` 后继续安装，nil 即跳过策略层 | **fail-closed**：nil → 503，不得继续；调用方负责启动期注入断言。门控：`make no-backdoor-check`（PolicyGate fail-closed） |
| R1.15 | MCP 子进程继承完整父环境 | `cmd.Env = os.Environ()`，宿主 `*_KEY/_TOKEN/_SECRET/_PASSWORD` 泄入 stdio 子进程 | 统一 `sanitizeParentEnv()`，仅留运行必要变量并叠加显式 `MCPClientConfig.Env`。门控：`Test_inv_MCPSubprocessEnvSanitized` |
| R1.16 | 持有 DB 连接期间发起阻塞式外部调用 | 未 `rows.Close()`/`tx.Commit()` 前调 LLM 推理、网络请求；writer `MaxOpenConns=1`（`internal/store/store.go`）会连带卡死其他读写 | 先读完 / Close / Commit（或 Rollback）释放连接，再调外部；必要时"读事务 → 无锁外部调用 → 写事务"。门控仅覆盖嵌套 Rows 形态（`Test_inv_NoNestedRowsQuery`），其余靠自查 |

**R1.4 例外**：M7（`internal/tool/`、`internal/action/`）的核心接口（`Tool`/`CapabilityLevel`/`ToolRegistry`/`Catalog` 等）可定义在实现方包内，前提是消费方为明确的单向上游层（如 `internal/agent/`，见 `docs/arch/00-Global-Dictionary.md §7`）。无真实消费方的接口声明视为死代码，必须删除，不得借此例外保留。新增 producer-side interface 前先 grep 确认消费方存在且位于允许的上游层，否则归口 `internal/protocol/`。

## R2 命名规范字典

> 同一概念全仓库唯一词根。违反视为 R1 反模式扩展。同文件常量用 `const ( ... )` 块分组，不加 `=` 对齐空格。

### R2.1 结构骨架

| 层/类型 | 模式 | 示例 |
|---------|------|------|
| Package | 单数小写，简短 | `storage`, `inference`, `policy` |
| Interface | consumer-side, 动词+er | `Store`, `Provider`, `EventLogger` |
| Struct | PascalCase | `MemoryEntry`, `SQLiteBlackboard` |
| 构造函数 | `NewXxx(deps)` | `NewOrchestrator(bb, registry, maxAgents)` |
| 公开方法 | 动词短语 | `FindByID`, `Save`, `DispatchPending` |
| 私有方法 | camelCase | `dispatchPending` |
| 常量 | PascalCase 按类型分组 | `LayerWorking`, `TaskPending`, `TaintHigh` |
| Test func | `TestXxx_Scenario` | `TestStore_InsertDuplicate` |
| 文件名 | snake_case | `factuality_guard.go` |
| 测试文件 | `_test.go` 与被测同级同目录 | `factuality_guard_test.go` |
| Git 提交 | `<type>(<scope>): <简体中文>` | `feat(storage): 实现 SurrealDB-Core HNSW` |

### R2.2 动词词根（避免 `GetSkill` / `FetchSkill` / `LoadSkill` 并存）

| 语义 | 词根 | 禁用 |
|------|------|------|
| 读单条 | `Get` | Fetch / Load / Retrieve |
| 读多条 | `List` | Query / Find |
| 按条件读 | `FindBy<Field>` | Search / Lookup |
| 写新 | `Create` 或 `Insert` | Add / New（`New` 仅构造函数） |
| 改 | `Update` | Modify / Edit / Patch |
| 删 | `Delete` | Remove / Drop（`Drop` 仅 schema） |
| 持久化 | `Save` | Persist / Store（`Store` 是名词，存储引擎） |
| 触发动作 | `Dispatch` / `Trigger` | Fire / Emit（`Emit` 专用于 events 写入） |
| 写事件 | `EmitEvent` | LogEvent / WriteEvent |
| 校验 | `Validate` | Check / Verify（`Verify` 专用密码学） |
| 评估 | `Evaluate` | Assess / Score |
| 注册 | `Register` | Add / Bind |

### R2.3 名词词根

| 概念 | 词根 | 禁用 |
|------|------|------|
| 任务 | `Task` | Job / Work / Action（`Action` 是工具调用专用） |
| 计划 | `Plan` | Workflow / Pipeline（`Pipeline` 仅指 `Staging-Pipeline`） |
| 技能（Python 蒸馏，ContainerSandbox，ADR-0008） | `Skill` | Capability / Function |
| 工具（执行边） | `Tool` | Action / Operation |
| 记忆 | `Memory` | Cache / Store |
| 黑板 | `Blackboard` | SharedState / Bus |
| 智能体 | `Agent` | Worker / Actor |
| 凭证 | `Credential` | Secret / Token（`Token` 仅指 `Capability Token` / `TokenBurnRate`） |
| 提供方 | `Provider` | Vendor / Service |
| 注册表 | `Registry` | Manager / Catalog |
| 路由器 | `Router` | Dispatcher / Switch |
| 守卫 | `Guard` | Validator / Filter |
| 网关 | `Gateway` | Bridge / Proxy（`Proxy` 仅指网络代理） |
| 配置 | `Config` | Settings / Options（`Options` 仅函数选项模式） |

### R2.4 量纲后缀

| 量纲 | 规则 | 示例 |
|------|------|------|
| 时长 | `time.Duration` 类型，不带后缀 | `timeout`, `interval` |
| 时间戳 | `At` 或 `Time` 后缀 | `createdAt`, `expireTime` |
| 字节 | `Bytes` 后缀 | `payloadBytes` |
| 速率 | `Rate` 后缀 | `TokenBurnRate` |
| 计数 | `Count` 后缀或复数 | `errorCount`, `events` |
| 阈值 | `Threshold` 后缀 | `surpriseThreshold` |
| 分数/比率 | `Score` 或 `Ratio` | `confidenceScore` |
| 限额 | `Limit` 或 `Max<X>` | `MaxAgents`, `requestLimit` |

### R2.5 错误码命名

格式 `Code<Category>`，唯一定义处 `pkg/apperr/apperr.go`（禁止在其他包新增 Code 常量）。现有值：通用 `CodeOK/Internal/InvalidInput/Unimplemented`；资源 `NotFound/AlreadyExists/Conflict/ResourceExhausted`；权限 `Unauthorized/Forbidden`；`Timeout/Cancelled`；`ProviderExhausted/NetworkUnavailable`；`StorageUnavailable`；`TaintViolation`；`SandboxTier0Limit`。新增前必须 `grep -n "Code.*Code = " pkg/apperr/apperr.go` 检查同义码；重复定义视为 R1 扩展。

### R2.6 指标命名

格式 `polaris_<subsystem>_<name>_<unit>`。subsystem：`inference/storage/observability/policy/cognition/action/swarm/governance/edge`；unit：`total/count/bytes/seconds/ratio/rate`（无单位 Gauge 可省略）。示例：`polaris_inference_tokens_total`、`polaris_storage_write_latency_seconds`。

## R3 HE-Rules 可验证工程量表

HE-1~7 的工程化实现（速查见根 `CLAUDE.md §不变量`）。AI 每写完一段代码必须逐条自查。

### R3-HE1 可观测优先

自查：这条路径可追溯吗？有 TokenBurnRate / SurpriseIndex 埋点吗？

- 每次 LLM 调用：`InstrLLMCallsTotal.Add` + `InstrLLMLatencyMs.Record`；每次 Tool 调用：`InstrToolCallsTotal.Add` + `InstrToolLatencyMs.Record`
- 每次 Retrieval 返回：填充 `ExplainBits` 并 `RecordExplainBit(ctx, "BM25"/"Vector"/...)` 上报
- 新常驻业务路径：在 `internal/observability/metrics/instruments.go` 注册指标
- 禁止：能算但不上报的中间状态、无埋点的设计

### R3-HE2 可验证执行

自查：安全边界是物理断裂还是概率过滤？Taint 显式传播了吗？

- 工具入口：`SchemaValidateInterceptor` 在 `Dispatcher.Use(...)` 管道首位注册
- 对象修改操作：必经 `PolicyGate.IsAuthorized()`（fail-closed）；安全门自身 nil → 503（R1.14）
- 结果 `TaintLevel` 只升不降（ADR-0007 PropagateTaint）
- 禁止：概率过滤当安全（R1.10）；Taint 被截断或静默丢失；`CodeInvalidInput` 不包装直接透传下游

### R3-HE3 可组合原语

模块通信分两层：热路径（延迟敏感）用 Go interface；冷路径与跨模块状态变更用结构化事件。

- 新接口定义在 `internal/protocol/`（R1.4）
- 跨模块通信走 `MutationBus` / `EventLog`，禁止直接调用对方包内改变函数
- 新工具经 `protocol.ToolRegistry.ExecuteTool` 统一入口，不开旁路
- 禁止：service 调 DAO、controller 含业务逻辑、字符串隐式耦合

### R3-HE4 数据驱动迭代

自查：这个变更有 Eval 可验证吗？

- 新功能定义 `EvalCase`（或复用），`make test` 含回归索引
- Prompt 改动附前后 Eval 对比；RAG 改动须有切片、Embedding、检索权重的实验依据
- 禁止：跳过 Eval 直改阈值 / Prompt / 权重，"看起来更好"不算上线条件

### R3-HE5 状态机持有控制流

Go 确定性 FSM 持控制流，LLM 仅做概率性填空。

- LLM 调用出现在 FSM 状态 `handler` 内，不存在裸 `callLLMAndWait`；状态跳转只经 `FSM.Trigger()`，禁止直接赋值状态字段
- 耗时操作（扩展激活、反射写回）移出锁范围，用 `SafeGo` 异步（`09 A-09`）；多 Provider 并发必须全量遍历（`09 A-04`）
- 禁止：无 FSM 包裹的 `while true { callLLM }`；LLM 返回内容直接驱动巨大 `goto`/`if-else` 分支树（应先导出结构化模型再进 FSM）；FSM Handler 内裸起 goroutine 调 LLM 后回写共享字段（竞态）

### R3-HE6 状态落盘

崩溃恢复从 EventLog 回放。

- 模块间状态变更经 `MutationBus.Submit()` 汇入单写者
- Agent 状态转移后显式写 EventLog（`runFSM` 每次转移后触发）
- 崩溃恢复可从 `events` / `outbox` 表重建执行状态
- 禁止：状态仅存内存；用独立 checkpoint 文件替代 EventLog（ADR-0006）；DB 连接期间调 LLM（R1.16）

### R3-HE7 防退化边界（ADR-0112）

自查：是否绕过 ExecuteTool 写记忆？是否弱化五防线任一条？安全门是否 fail-closed？

- 记忆写入只经 Memory-Write-Tool / `ExecuteTool`，不开旁路（标杆见 `07 §7.1`）
- Taint / Cedar / KillSwitch / SSRFGuard / 能力令牌任一防线不得弱化；安全门 nil → 拒绝（R1.14）
- 禁止：绕过 ExecuteTool 写记忆；为通过测试放宽防线；安全门异常时放行

> Agent 工程化陷阱（A-01~A-14）与生产原则（P-1~P-9）见 `09-LLM-Agent-Production.md`。

## R4 Tier-0-Limit 兜底

新特性必须自问"8GB RAM 能跑吗"：不能 → 置于 FeatureGate 后，Tier0 关闭、Tier1+ 打开；高资源消耗不得作默认路径。

## R5 注释规范

中文短句，只写"为什么"（设计意图、约束、陷阱、非显而易见的行为），不写"是什么"。代码注释 `// 中文短句`，文档注释用完整句子。

## R6 模块引用纪律

- `internal/` 各包按 L0←L1←L2←L3 单向引用，禁止反向
- `pkg/` 无业务逻辑，可被任意层引用
- 跨模块通道与 FFI 协议见 `04-Module-Boundary.md B1 / B2 / B4 / B5`

## R7 可读性硬上限

| 维度 | 上限 | 处置 |
|------|------|------|
| 函数体行数 | ≤ 60（审查项，无门控） | 超出拆分，除非 ADR 豁免 |
| 文件行数 | ≤ 800（软），≤ 850（硬） | 801–850 容差区门控只告警；> 850 必须按职责拆文件 |
| 嵌套深度 | ≤ 3 | early return / 提取命名函数 |
| 圈复杂度 | ≤ 15 | 拆分 + 表驱动 |
| 函数参数数 | ≤ 5 | struct 参数包 |
| 包内文件数 | ≤ 20 | 考虑拆子包 |

门控：`.golangci.yml`（`gocyclo` / `nestif` / `lll`，CI fail-closed）；文件行数 `Test_inv_FileLineLimit`。`funlen` 不启用（与 `gocyclo` 冗余、误报高，ADR-0091）。Fail-Closed 安全判定、身份单源、后台协程生命周期锚定等规范与 Lint 门控见 ADR-0087（原 0094）。


## R8 参考实现强制引用

写任何新代码前，先 Read `07-Reference-Implementation.md` 中对应模块的标杆文件。

- PR 描述必填：`参考实现: internal/xxx/yyy.go | 对齐 / 偏离原因`；偏离协议见 `07 §7.3`
- 1 提交 = 1 逻辑变更；非测试 diff > 1000 行的提交，提交信息须引用 ADR 或计划文件（审查项，无门控；ADR-0112）
- 为什么：无锚点时 AI 每次现编风格，同一模块三种实现并存；标杆是消除"局部连贯/全局混乱"的物理对齐手段
