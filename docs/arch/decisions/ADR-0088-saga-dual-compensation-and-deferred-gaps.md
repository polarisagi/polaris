# ADR-0088: Saga 双补偿归属裁决暂缓（先幂等止血）+ 三项对标差距的处置边界

- **状态**: Accepted（决策一止血部分已实施）/ Deferred（决策一归属裁决、决策三） | **日期**: 2026-08-06 | **模块**: `internal/execute/dag`, `internal/agent/fsm`, `internal/agent/context`, `internal/automation/hitl`

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

`internal/protocol/saga_ledger.go`、`internal/execute/dag/executor_node.go`、`internal/agent/fsm/state_machine_effects.go`、`internal/security/token/capability_token.go`、`internal/action/capability_token.go`、`internal/action/codeact/code_act.go`、`internal/automation/hitl/gateway.go`、`internal/observability/metrics/instruments.go`
