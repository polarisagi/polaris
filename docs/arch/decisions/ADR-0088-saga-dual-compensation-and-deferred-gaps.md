# ADR-0088: Saga 双补偿归属裁决暂缓（先幂等止血）+ 三项对标差距的处置边界

- **状态**: Accepted（四条决策均已实施） | **日期**: 2026-08-06（同日补充终态裁决） | **模块**: `internal/execute/dag`, `internal/agent/fsm`, `internal/execute/orchestrator`, `internal/agent/context`, `internal/automation/hitl`, `internal/memory`

## 决策一：两套并行 Saga 补偿——先加幂等键，归属裁决暂缓

系统存在两条数据来源互不相干的 Saga 补偿路径，同一次执行失败会**各跑一遍**：

| 路径 | 补偿动作来源 | 执行点 |
|---|---|---|
| A | DAG 节点声明的 `node.Compensation`（LLM 生成计划时填写，`S_VALIDATE` 强制副作用节点必填） | `DAGExecutor.runCompensation`，节点失败后同步逆序执行 |
| B | 工具注册表的 `toolDef.UndoFn`（`agent_execute_dag.go` 在每次工具调用成功后追加进 `sCtx.SagaLog`） | FSM `S_EXECUTE → S_ROLLBACK` 转移的 `rollbackSaga` Effect |

一个既在 DAG 里声明了 `Compensation`、又在注册表里登记了 `UndoFn` 的副作用工具，其 undo 会执行两次。对"删除文件 / 退款 / 撤回消息"这类**非幂等**补偿，第二次 undo 作用在已被撤销的状态上，属数据损坏级缺陷。

**本次只做止血**：引入 `protocol.SagaCompensationLedger`，以 `(工具名 + 参数哈希)` 为键，同一补偿动作只执行一次。账本由 `runExecuteDAG` 每次新建，经 ctx 交给 A、经 `StateContext.SagaLedger` 交给 B。

三个刻意的设计取舍：

- **键用 (工具, 参数) 而非节点 ID**：两条路径对同一补偿的命名不同（A 用 `Compensation.ToolName`，B 用 `toolDef.UndoFn`，且 B 的 `NodeID` 是用 toolName 顶替的），只有"最终执行哪个工具、带什么参数"在两侧一致。反过来，若两侧的工具或参数确实不同，说明是语义不同的两个补偿，本就该都执行——键的粒度恰好表达这个判断。
- **nil 账本 fail-open**：未注入时恒放行，回到修复前行为。这里刻意不 fail-closed——账本缺失导致"多补偿一次"是已知的旧状态，而"漏补偿"会留下未回滚的副作用，后者更糟。
- **账本生命周期是"每次 `runExecuteDAG` 一份"**：它表达的是"本轮补偿里某个 undo 是否已跑过"。跨轮复用会让 `S_REPLAN` 后新一轮的补偿被错误跳过。

**暂缓的是归属裁决**：A/B 应合并到单一 SSoT，还是明确划分各自负责的副作用类别。该裁决需要先回答"`node.Compensation` 与 `toolDef.UndoFn` 在语义上是否本就该是同一件事"，涉及 M04 计划生成契约与 M07 工具注册契约两侧，超出缺陷修复批次范围。

**重新评估触发条件**：出现 A/B 补偿语义确实需要分别执行的真实用例（届时幂等键粒度需重新论证）；或 `node.Compensation` 与 `toolDef.UndoFn` 的填写规则出现文档级冲突。

### 反例守护

不得以"S_ROLLBACK 是空过渡态"为由删除 FSM 的 `S_ROLLBACK` 状态——`rollbackSaga` 是该转移上挂载的真实 `DeterministicEffect`，且 `S_ROLLBACK` 有两条语义不同的出边（`RollbackDone → S_REPLAN` 带 replanCount Guard、`RollbackPartial → S_FAILED`）。2026-08 的一份外部审查报告曾以此为由建议删除，前提不成立。

不得在未裁决归属前，单方面删除 A 或 B 中的任一条路径。

## 决策二：`MaxCallsPerTask` 必须有显式消费入口，不得只签发不兑现

能力令牌的 `Claims.MaxCallsPerTask` 此前在全仓只有一处读取点（`agent_execute_dag.go` 从 `protocol.CtxCapabilityToken` 取 token 后比对），而该 ctx 键**从未被任何生产代码写入过**——字段签发后从未被兑现，"一次性令牌"始终只存在于文档表述中（M07 §4.6、`NewJITToken` 注释、调用点注释三处）。同时 `NewJITToken` 对 `Mint` 硬编码 `maxCallsPerTask=0`（语义为无限制），把调用方传入的 `TokenOperation{MaxCalls: 1}` 整个丢弃。

**决策**：`Verify` 只承载签名/过期/撤销这类**无状态**判定；"还剩几次可用"是有状态语义，必须由独立的 `TokenManager.Consume(tokenID)` 承载，并由真正放行副作用的入口（`CodeAct.validatePolicyAndEnv`）在执行前调用。多操作令牌的上限取各操作中最紧的一个（与 `minTTL` 取最短 TTL 同构，最小权限原则）。

### 反例守护

不得把使用次数校验塞进 `Verify`——`Verify` 在多处被当作纯函数式校验反复调用，混入副作用会导致计数被无关调用点消耗。

不得再新增"签发了某个 claim 但无人读取"的字段：能力令牌的每个 claim 都必须有明确的兑现点，否则它提供的是安全假象。

## 决策三：GD-14-005 工作区上下文自动装载——暂不实现，信任模型先行

业界 `AGENTS.md` / `CLAUDE.md` 约定（Agent 自动装载工作目录内的约束文档）**暂不实现**。

暂缓理由不是工程量——探测 + 读取 + 注入确实只有几十行——而是**信任模型未定**。把工作区文件内容装进 `ZoneImmutable` 等于让用户目录里的文件获得系统提示词级别的信任。Agent 处理一个 clone 下来的仓库时，其中的 `AGENTS.md` 是攻击者可控的，这是一条直通最高信任区的提示注入通路，正好绕过 HE-2 所依赖的确定性边界。

三个候选方案及其代价，需产品 + 安全共同裁决后再立项：

| 方案 | 安全性 | 代价 |
|---|---|---|
| 装入 `ZoneImmutable` | 最差：等同系统提示，注入直达 | 功能最强，与业界约定一致 |
| 装入普通 Zone + `TaintExternal` | 好：走既有污点管线，高污点内容触发既有防线 | 失去"强制约束"语义，模型可忽略 |
| 仅对配置中显式声明信任的工作区路径启用 `ZoneImmutable`，其余走 `TaintExternal` | 折中：信任决定权交回用户 | 需新增配置面与路径匹配逻辑 |

**重新评估触发条件**：上述信任模型三选一有明确结论；或出现"用户已在配置中显式声明信任路径"的产品形态。

### 反例守护

不得以"业界都这么做""代码量很小"为由绕过信任模型裁决直接实现。任何实现都必须显式声明装入哪个 Zone、打什么污点等级。

## 决策四：GD-14-004 HITL 审批疲劳——观测先行，本轮不改放行逻辑

自适应降级（"同一 Agent 对同类工具的申请在过去 24h 内已获多次批准且 SurpriseIndex 低 → 自动降级为通知或静默放行"）**本轮不实现**。

理由：该机制本质是在削弱安全边界，而当前系统连审批频次与批准率的埋点都没有（`internal/automation/hitl` 与 `internal/security/policy` 中不存在任何频率或信任评分统计）。在没有真实数据的前提下拍阈值，等于凭感觉打开一个越权口子。

**本轮落地的是观测**：`polaris.hitl.prompts_total`（labels: checkpoint_type, agent_id）与 `polaris.hitl.decisions_total`（labels: checkpoint_type, decision, source）。其中 `source=human` 且批准率长期接近 100% 的 `checkpoint_type`，就是审批已退化为习惯性点击的候选证据。

`agent_id` 维度刻意传空串：Agent ID 形如 `agent-{sessionID}`，直接打标签会造成时间序列爆炸（与 `InstrPIIMappingEvictionsTotal` 不打 partitionKey 同一原则）。按 Agent 下钻走审计表，不走指标维度。

**重新评估触发条件**：积累到足以支撑阈值的真实分布数据（至少覆盖各 `checkpoint_type` 的正常运行周期）。

### 反例守护

不得在无数据支撑的情况下引入任何"自动降级为通知"的放行路径。`resolveTimeoutAction` 中 TaintLevel ≥ 2 一律 `auto_deny` 的地板不受本决策影响，任何自适应机制都不得穿透它。

## 引用代码

`internal/protocol/saga_compensation.go`（`SagaCompensationLedger` 实际定义处，原引用文件名 `saga_ledger.go` 不存在）、`internal/execute/dag/executor_node.go`、`internal/agent/fsm/state_machine_effects.go`、`internal/security/token/capability_token.go`、`internal/action/capability_token.go`、`internal/action/codeact/code_act.go`、`internal/automation/hitl/gateway.go`、`internal/observability/metrics/instruments.go`

---

## 终态裁决（同日补充，取代上文三处 Deferred）

上文四条决策中原本标记 Deferred 的部分已全部裁决并落地，此节记录最终结论。

### 决策一终态：`ExecNode.Compensation` 是 Saga 补偿的唯一 SSoT

裁决依据是一条全仓核查事实：`ToolDefinition.UndoFn` **从未被赋值过**（tool.yaml 加载器无对应字段映射，唯一出现处是 `agent_execute_dag.go` 读取它）。因此 `sCtx.SagaLog` 恒为空，`fsm.rollbackSaga` 恒遍历空切片、恒返回 `S_ROLLBACK_OK`——FSM 的 `S_ROLLBACK` 自始至终没有反映过任何真实补偿结果。所谓"两套并行补偿"实为"一套活的 + 一套结构上死的"。

B 之所以删除而非接线补活：`UndoFn` 是挂在**工具定义**上的静态工具名，没有参数绑定。补偿"删除刚创建的那个文件"需要知道是哪个文件，而工具定义层拿不到本次调用的实参——这是设计上的死胡同，不是接线缺口。

落地：删除 `SagaLog` / `types.SagaStep` / `ToolDefinition.UndoFn`；`rollbackSaga` 从"执行者"改为"结果汇报者"，读 `SagaCompensationRecorder` 决定 `S_ROLLBACK_OK` / `S_ROLLBACK_PARTIAL`。上文提到的幂等键账本（`SagaCompensationLedger`）随之删除——单一路径不需要跨路径去重，它本就是临时止血。

`S_ROLLBACK` 状态本身保留（两条语义不同的出边），反例守护不变。

### 决策三终态：工作区上下文协议按"默认不可信 + 显式信任例外"实现

三个候选方案中选定第三个（折中）：默认按 `TaintHigh` 进 `ZoneExternalCatalog` 并加 Spotlighting 围栏，与 S-02 的第三方扩展自述同级——两者威胁模型完全一致（第三方可控、看起来像指令的文本），不应给工作区文件更高信任度。仅当工作区路径命中用户在 `[agent] trusted_workspace_roots` 中**显式声明**的绝对路径前缀时，才写入 `ZoneImmutable`。

信任判定的两个必须项：前缀匹配须校验目录边界（否则 `/home/u/proj-evil` 会继承 `/home/u/proj` 的信任）；相对路径条目一律忽略（`"."` 之类会让任意 cwd 变成受信任）。二者均有回归测试锁定。

### 决策四终态：自适应降级实现为"默认关闭 + 硬地板 + 只降一档"

观测（`polaris.hitl.decisions_total`）回答"**是否需要**开启"，`TrustScorer` 提供"**开启后**怎么执行"，两者互补而非二选一。

不可放宽的设计约束：默认 `MinApprovals=0` 即完全关闭；启用后**只降级为通知**，永不静默放行；污点 ≥ TaintMedium / RiskLevel ≥ 3 / 设备操控 / L4 晋升 / 无法归因到具体 Agent 一律不参与（`downgradeEligible`，与 `resolveTimeoutAction` 的地板保持同一组条件）；任何一次人工拒绝立即清零信任；只有**人工**决策参与累积——把自动放行计入会形成正反馈，几轮后就没人在看了。

### 新增决策五：跨 Agent Saga 协调补齐并发扇出路径

`StateGraphExecutor`（Parallel / MapReduce / Sequential 的共同底座）一直声明并校验 `WorkflowNodeSpec.Compensation`，却从未在任何失败路径上执行过补偿——节点失败时只返回错误，已成功兄弟节点的副作用无人回滚。而"部分成功部分失败"恰是并发扇出的常态，不是边界情况。

落地两件事，顺序不可交换：**先取消在途兄弟**（`Blackboard.CancelTask`：先 cancel goroutine 再置 DB 状态），**再逆序补偿**。反过来的话，仍在执行的兄弟会在补偿完成之后才写入副作用，导致"补偿先于副作用"，回滚等于没做。

三条补偿路径（Pipeline / PatternDAG / StateGraph）共用同一个 `PipelineOrchestrator.monitorCompensationTask`，即"补偿任务自身超时/失败 → ESCALATE"的处置只有一份。

### 新增决策六：遗忘按"有没有人用"而非"够不够旧"淘汰

纯时间衰减（`salience × exp(-rate·age)`）表达的是"越老越该忘"，会误删**旧但持续有用**的记忆（项目约定、用户长期偏好、反复踩过的坑），同时留下大量"新但从没人用过"的噪声——与 GD-14-003 想提升的检索信噪比恰好相反。真正该淘汰的是"没人用的"，不是"旧的"。

`episodic_events` 新增 `retrieval_count` / `last_retrieved_at`，`UpdateDecayReinforced` 叠加两项信号：命中次数的对数增益（封顶 3.0，避免高频记忆获得永久豁免）与超期闲置惩罚（14 天起）。

性能硬约束：命中统计**绝不在读路径上同步写库**。检索是高频只读操作，每次命中都 UPDATE 会在 Tier-0 单写者 SQLite 上与主写入链路争锁。采用内存累计 + 5 分钟批量落盘，刻意与 6h 遗忘周期解耦——遗忘跑之前必须已看到最近命中，否则刚被反复使用的记忆会因"库里仍是 0 次命中"被误判为噪声。

### 反例守护（补充）

- 不得恢复 `ToolDefinition.UndoFn` 那套补偿机制；需要补偿的工具在 DAG 节点上声明 `Compensation`。
- 不得让 `rollbackSaga` 重新承担执行职责——它只汇报 `execute/dag` 的结果。
- 工作区上下文的任何实现都必须显式声明装入哪个 Zone、打什么污点等级；不得因"文件名恰好是约定名字"推定信任。
- 自适应降级的硬地板与 `resolveTimeoutAction` 必须同步修改；出现"超时不敢自动放行、但疲劳降级放行了"的组合即为缺陷。
- 检索强化不得改为读路径同步写。

---

## 决策七：存量债的处置边界（2026-08-06 复核）

本节记录一轮存量债复核的结论，避免后续每次审查都重新讨论同一批条目。

### errcheck 门控扩至全部 internal 模块

此前只接入 8 个模块，其余 20 个既不扫描也不算豁免——处于"门控看不见"的状态。而 GR-4-00x 那批 outbox/DB 写入吞错恰好就在未接入的 `internal/agent` 里，是靠人工逐个翻出来的：**本该被机械拦住的东西靠人去找**，这正是门控覆盖面本身该被当作一等问题看待的理由。

接入后暴露 88 处，逐个分诊后手工修复 15 处（`security/guard` 的 3 处 crypto/rand、`security/killswitch` 的 MkdirAll 与通知失败、`agent` 中断投递 3 处、`vfs` 3 处、`agent/fsm` 的 hash.Write 4 处），其余 73 处登记 baseline（148 → 221，差额与手工修复后剩余数精确相符）。

**baseline 是待偿债务清单，不是永久许可**。棘轮只挡新增、不追溯旧账；条目应随后续批次逐步清零，不得因"反正已登记"而长期沉淀。

### deadcode 白名单 20 条：全部有效，无需处置

逐条核验其指向的函数仍真实存在（陈旧条目会静默失效，等于悄悄放宽门控）。分类：13 条测试假阳性（仅被单测调用、无自然生产调用点）、2 条 tier1 feature 门控假阳性、1 条 ADR-0007 刻意保留的降级器、1 条 ADR-0069 保留的手动注册入口、其余为白盒测试访问器。均有据可查。

### 行数上限 baseline：随修随退

`internal/knowledge/retriever.go` 因 GD-13-002 收敛已降至 357 行，其 baseline 条目本轮移除——**降到限内就必须退出 baseline**，否则棘轮会在该文件上永久失效。其余 15 项均仍超限且各有拆分理由记录。

### `internal/bootstrap` 零引用：维持现状，不删不接

`internal/bootstrap`（373 行，`Bootable` + Kahn 拓扑排序 + 四阶优雅关停）定义的是**目标契约**，生产实际走 `cmd/polaris/boot_*.go` 的手工装配链（4015 行生产代码）。ARCHITECTURE.md §8.2 已完整记录该事实。

> 本节初稿曾称 §8.2 "点明了它与 `Module-Dependency-Axioms.md §4` [MUST] 表述的冲突"。该 [MUST] 已于 2026-07-28 改为"必须且仅能在**装配层**完成"并加注物理落点，冲突随之消失；§8.2 相应段落已于 2026-08-07 订正。**接线与否不影响这两份文档的一致性**——不得再以"消除文档冲突"为由推动接线。

三个选项的取舍：

- **删除** → 移除 ARCHITECTURE.md 与 Axioms [MUST] 共同指向的目标契约，等于把"装配应当收敛"的设计意图一并抹掉，文档随之失去落点。
- **接线** → 需要把 4015 行手工装配链整体迁移到 Bootable/Kahn 之上。启动顺序牵动每个子系统的初始化依赖，回归面覆盖全部 28 个模块，属于必须独立立项 + 分阶段迁移的改造，不是缺陷修复批次能夹带的。
- **维持** → 事实已在架构文档中如实记载，无隐藏漂移；deadcode 门控不报（它只扫函数可达性，不判包级引用）。

选择维持。**重新评估触发条件**：启动顺序出现真实缺陷（如某子系统初始化依赖被手工链遗漏）；或手工装配链再次显著膨胀（> 6000 行）使人工维护顺序变得不可靠。

### `internal/bootstrap` 接线可行性复核（2026-08-07）

一次以"立即接线"为目标的复核，结论仍为维持不接线。本节记录复核所得的硬事实，避免下次再从零讨论。

**触发条件逐条实测：四个全部未满足。**

| # | 条件 | 出处 | 实测 |
|---|---|---|---|
| ① | 启动顺序出现真实缺陷 | 本 ADR 上节 | 无案例 |
| ② | 手工链 > 6000 行 | 本 ADR 上节 | **4015 行**（`cmd/polaris/boot_*.go` 生产文件；连 5 个测试文件一并计为 4594，勿混用口径） |
| ③ | 模块数增长到手工排序易错 | ARCHITECTURE.md §8.2 | 仍为 6 阶段线性链，依赖由函数签名静态可验 |
| ④ | 需按 Tier/FeatureGate 动态增删模块 | ARCHITECTURE.md §8.2 | 无此需求 |

**契约与实际装配的结构性不匹配（本次复核的主要产出，此前无处记载）。**

前两项是**语义上无法表达**，不是工作量问题——即便投入任意多人力也无法在现有 `Bootable` 契约下正确表达：

- **两阶段装配 vs 单阶段契约**：`Bootable` 只有 `Init` 一个构造钩子，而实际装配普遍是"先构造、后回注"——`cmd/polaris/boot_*.go` 生产文件中 setter/回注型调用 **137 处**（`.Set*` 107、`.On*`/`.Register*` 30）。其中存在**反向回注**，典型如 `boot_agent.go:411` `tb.RecoveryHandler.SetBlackboard(blackboard)`：L1 tools 阶段的对象被 L2 agent 阶段的产物回填。拓扑排序保证"被依赖者先 Init"，反向回注天然违反拓扑方向。`DependencyMap` 表达的是"谁需要谁"，无法表达"谁必须在谁之后被回填"。
- **顺序性副作用 vs 依赖图**：崩溃恢复（`main.go:154-155`）依赖进程级全局标志 `protocol.ReplayMode` 的**独占窗口**——必须在 Provider 加载后、`bootServer` 前串行跑完，但它与 `bootServer` 之间**无依赖边**。Kahn 排序对无边节点不保证相对顺序（当前实现按 map 迭代填初始队列）。同类还有 `ab.Supervisor.Start()` 必须在两个 defer 就位后（`main.go:137-140`）、`eval --ci-gate` 分支在 `bootServer` 前 return（契约无"条件性跳过后续模块"语义）。用假依赖边强制定序可绕过，但假边无法与真依赖区分，比手工排序更难维护。

后三项是**契约自身缺陷**，可修，但修了也不解决上面两项：

- `gracefulShutdown` 四阶各自 `for name, mod := range b.modules`，**map 迭代顺序随机**。现有链有硬时序：`Supervisor.Stop()` 先于 `ReaperStop()`，`ReaperStop()` 早于 dbWriter 排空（`main.go:137-138,213`）。接线即把确定序换成随机序。
- `Init(deps *DependencyMap) error` **无 ctx**，而 7 个 boot 阶段签名全部带 ctx；`bootSubstrate(ctx, stop)` 还额外消费 `context.CancelFunc` 控制流句柄。
- `Ignite` **无失败回滚**：中途 Init 失败直接返回，已初始化模块不走任何 Stage。现有链靠 defer LIFO 兜底，其中 `tb.PersistentSandbox.Shutdown()` 是 ADR-0008 要求的 L4 解释器子进程终止点，缺失即产生孤儿进程。

另：`internal/bootstrap` 单测仅 1 个用例（`TestGracefulShutdown_WithErrors`，只验证出错不 panic），`topologicalSort`／`Ignite`／KMS memclr **零覆盖**。

**本次不修上述缺陷。** 按 ADR-0062 deadcode 治理纪律，为一个零引用且已判定不接线的包投入修复 + 补测成本理据不足，会退化为"为将来可能用而现在维护"。缺陷记录在案即可。

**将来立项时的 P0 前置**：先修三项契约缺陷（关停按 Ignite 拓扑逆序、`Init` 加 ctx、`Ignite` 失败时按已完成逆序调用 `Stage4Closer`）并补齐 `topologicalSort`／`Ignite` 单测，再谈迁移；且必须先给出两项"无法表达"问题的处置方案（新增二阶段回注钩子？假依赖边并逐条记录真实原因？），否则迁移在技术上做不完整。另需注意 `Bootstrapper.ListenAndServe` 自建 `signal.Notify` 与 `main.go` 既有两套信号处理（`signal.NotifyContext` + TripleCtrlCGuard 的 SIGINT 连续计数）不能共存。

### 反例守护

不得因"已登记 baseline/白名单"就认为条目已处理——它们是债务记录，处置状态是"待偿"。

不得在未逐条核验有效性的情况下重新生成 deadcode 白名单或 errcheck baseline：陈旧条目不会报错，只会静默放宽门控，这比没有门控更危险（它营造了被覆盖的假象）。
