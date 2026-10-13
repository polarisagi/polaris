# ADR-0088: Saga 补偿单一 SSoT + 三项对标差距的处置边界

> 状态：Accepted（七条决策均已实施） ｜ 首次：2026-08-06（同日补充终态裁决） ｜ 最近复核：2026-10-12 ｜ 模块：`internal/execute/dag`、`internal/agent/fsm`、`internal/execute/orchestrator`、`internal/automation/hitl`、`internal/memory`

## 决策一：`ExecNode.Compensation` 是 Saga 补偿的唯一 SSoT

原有两条并行补偿路径（A：DAG 节点 `node.Compensation`；B：工具注册表 `toolDef.UndoFn` → `sCtx.SagaLog` → FSM `rollbackSaga`）会对同一失败各跑一遍，非幂等补偿（删文件/退款/撤回）有数据损坏风险。当日先加幂等账本止血，随后核查裁决：
- `ToolDefinition.UndoFn` 全仓从未被赋值，`SagaLog` 恒空，`rollbackSaga` 恒返回 `S_ROLLBACK_OK`——B 是"结构上死的"，不是接线缺口
- B 不可补活：`UndoFn` 是工具定义上的静态工具名、无参数绑定，补偿"删除刚创建的那个文件"拿不到本次实参

落地：删除 `SagaLog`/`types.SagaStep`/`ToolDefinition.UndoFn` 及止血用的 `SagaCompensationLedger`；`rollbackSaga` 改为结果汇报者，读 `protocol.SagaCompensationRecorder` 决定 `S_ROLLBACK_OK`/`S_ROLLBACK_PARTIAL`。`S_ROLLBACK` 状态保留（两条语义不同的出边：`RollbackDone → S_REPLAN` 带 replanCount Guard、`RollbackPartial → S_FAILED`）。

2026-09-20 复核：`ExecNode.Compensation` 字段保留，但 S_VALIDATE 不强制校验（`validator.go` 仅告警留痕）。理由：plan_dag schema/prompt 不产出 compensation，强制 = 系统瘫痪；inv_M4_06 禁止不可逆操作自动回滚，write_network 由 HITL 覆盖。Macro-DAG Saga 不受影响。

## 决策二：`MaxCallsPerTask` 必须有显式消费入口

能力令牌 `Claims.MaxCallsPerTask` 原仅一处读取点，且其 ctx 键从未被生产代码写入——"一次性令牌"只存在于文档；`NewJITToken` 还把 `TokenOperation{MaxCalls: 1}` 丢弃，硬编码 0（无限制）。
- `Verify` 只承载签名/过期/撤销等无状态判定；"剩余次数"由独立 `TokenManager.Consume(tokenID)` 承载，由放行副作用的入口（`CodeAct.validatePolicyAndEnv`）执行前调用
- 多操作令牌取各操作中最紧的上限（同 `minTTL`，最小权限）

## 决策三：工作区上下文自动装载（GD-14-005）——默认不可信 + 显式信任例外

`AGENTS.md`/`CLAUDE.md` 自动装载的风险不是工程量而是信任：装入 `ZoneImmutable` = 用户目录文件获得系统提示级信任，clone 下来的仓库即攻击者可控的注入通路，绕过 HE-2。
选定折中方案：默认按 `TaintHigh` 进 `ZoneExternalCatalog` 并加 Spotlighting 围栏（与 S-02 第三方扩展自述同级，威胁模型一致）；仅当路径命中 `[agent] trusted_workspace_roots`（`configs/defaults.toml`，默认空）显式声明的绝对路径前缀时才写入 `ZoneImmutable`。
- 前缀匹配须校验目录边界（否则 `/home/u/proj-evil` 继承 `/home/u/proj` 的信任）；相对路径条目一律忽略；二者有回归测试
- 否决方案：全部装入 `ZoneImmutable`（注入直达）；全部走 `TaintExternal`（失去强制约束语义，作为未信任路径的默认行为保留）

## 决策四：HITL 审批疲劳（GD-14-004）——观测先行 + 默认关闭的自适应降级

- **观测**：`polaris.hitl.prompts_total`（checkpoint_type, agent_id）、`polaris.hitl.decisions_total`（checkpoint_type, decision, source）；`source=human` 且批准率长期近 100% 的 checkpoint_type 即习惯性点击候选。`agent_id` 标签刻意传空串（`agent-{sessionID}` 会时间序列爆炸），按 Agent 下钻走审计表
- **自适应降级 `TrustScorer`**（`internal/automation/hitl/trust_score.go`）：默认 `MinApprovals=0` 完全关闭；启用后**只降级为通知，永不静默放行**；污点 ≥ TaintMedium / RiskLevel ≥ 3 / 设备操控 / L4 晋升 / 无法归因到具体 Agent 一律不参与（`downgradeEligible`，与 `resolveTimeoutAction` 地板同一组条件）；任何一次人工拒绝立即清零信任；只有**人工**决策参与累积（计入自动放行会正反馈）
- **实现载体**（2026-09-29，ADR-0104 决策五）：不持内存 map，直接查 `hitl_requests`（`044_hitl_requests.sql`）：同 `(checkpoint_type, agent_id)`、`decided_by='human'`、Window 内且晚于最近一次人工拒绝的批准行数 ≥ `MinApprovals`；降级放行落 `decided_by='trust_downgrade'`；查表失败不降级（fail-closed）；`RecordDecision` 已删（落账由 `Respond` 完成）

## 决策五：跨 Agent Saga 协调补齐并发扇出路径

`StateGraphExecutor`（Parallel/MapReduce/Sequential 底座）声明并校验 `WorkflowNodeSpec.Compensation` 却从未在失败路径执行补偿。落地两步，**顺序不可交换**：先取消在途兄弟（`Blackboard.CancelTask`：先 cancel goroutine 再置 DB 状态），再逆序补偿（否则补偿先于副作用，回滚落空）。Pipeline/PatternDAG/StateGraph 三条补偿路径共用 `PipelineOrchestrator.monitorCompensationTask`（补偿任务自身超时/失败 → ESCALATE，仅一份）。

## 决策六：遗忘按"有没有人用"而非"够不够旧"淘汰（GD-14-003）

纯时间衰减会误删旧但持续有用的记忆，并留下新但无人用的噪声。`episodic_events` 新增 `retrieval_count`/`last_retrieved_at`，`UpdateDecayReinforced` 叠加命中次数对数增益（封顶 3.0，防永久豁免）与超期闲置惩罚（14 天起）。
- 命中统计**绝不在读路径同步写库**（Tier-0 单写者 SQLite 争锁）：内存累计 + 5 分钟批量落盘（`retrieval_reinforcer.go`），与 6h 遗忘周期解耦——遗忘前必须已看到最近命中

## 决策七：存量债的处置边界（2026-08-06 复核）

- **errcheck 门控扩至全部 internal 模块**：原仅 8 个模块，其余 20 个"门控看不见"（GR-4-00x outbox/DB 写入吞错即在未接入的 `internal/agent`）。接入后手工修 15 处，其余登记 baseline。**baseline 是待偿债务清单，不是永久许可**，棘轮只挡新增，条目应逐批清零
- **deadcode 白名单**：逐条核验指向的函数仍存在（陈旧条目静默放宽门控）；分类为测试假阳性、tier1 feature 门控假阳性、ADR-0007 刻意保留的降级器、ADR-0093（原 0069）保留的手动注册入口、白盒测试访问器
- **行数上限 baseline 随修随退**：降到限内就必须移出 baseline（如 `internal/knowledge/retriever.go` 收敛后退出），否则棘轮在该文件永久失效
- **`internal/bootstrap` 已删除**：2026-10-13 删除 `internal/bootstrap` 与 `PhasedStartup`（ADR-0112 决策八 #1）；生产走 `cmd/polaris/boot_*.go` 手工装配链。重评触发：装配链 > 6000 行时重新评估装配方案，可从 git 历史取回。（原结论「维持现状，不删不接线」及下方 2026-08-07 复核记录保留为历史依据；2026-10 实测手工链 5805 行。）

接线可行性复核（2026-08-07）——重评触发条件（任一满足即重议）：启动顺序出现真实缺陷；手工链 > 6000 行；模块数增长到手工排序易错；需按 Tier/FeatureGate 动态增删模块。
- 两项结构性不匹配（语义上无法在现有 `Bootable` 下表达，非工作量问题）：① 两阶段装配（先构造后回注，`boot_*.go` 中大量 `.Set*`/`.On*`/`.Register*`，含反向回注如 `boot_agent.go` 的 `tb.RecoveryHandler.SetBlackboard`），与拓扑方向冲突；② 顺序性副作用（崩溃恢复依赖进程级 `protocol.ReplayMode` 独占窗口、`Supervisor.Start()` 须在 defer 就位后、`eval --ci-gate` 提前 return）无依赖边，假边无法与真依赖区分
- 契约缺陷：`Init(deps)` 无 ctx（仍未修）；关停 map 随机序与 `Ignite` 无失败回滚已于 2026-08-13 修复（GR-3-002/003）
- 将来立项 P0 前置：补 `Init` ctx 与 `topologicalSort`/`Ignite` 单测，并先给出两项"无法表达"问题的处置方案；`Bootstrapper.ListenAndServe` 自建 `signal.Notify` 与 `main.go` 既有信号处理（`signal.NotifyContext` + TripleCtrlCGuard）不能共存
- 不修上述缺陷：为零引用且判定不接线的包投入修复成本理据不足（ADR-0062 纪律）

## 反例守护

- 不得以"S_ROLLBACK 是空过渡态"为由删除该状态（2026-08 外部审查曾如此建议，前提不成立）
- 不得恢复 `ToolDefinition.UndoFn` 补偿机制；需补偿的工具在 DAG 节点声明 `Compensation`；`rollbackSaga` 不得重新承担执行职责
- 不得把使用次数校验塞进 `Verify`（多处被当纯函数反复调用，计数会被无关调用消耗）；不得新增"签发了 claim 但无人读取"的字段
- 工作区上下文实现必须显式声明 Zone 与污点等级，不得因文件名恰为约定名而推定信任
- 自适应降级的硬地板须与 `resolveTimeoutAction`（TaintLevel ≥ 2 一律 `auto_deny`）同步修改；出现"超时不敢放行、疲劳降级却放行"即缺陷；无数据支撑不得引入其他"自动降级为通知"路径
- 检索强化不得改为读路径同步写
- 不得因"已登记 baseline/白名单"认为已处理；不得未逐条核验就重新生成白名单/baseline

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-08-06 | 决策一~四初版（含 Saga 幂等账本止血、HITL 观测）；同日终态裁决：删除 B 路径、工作区信任折中、TrustScorer；新增决策五~七 |
| 2026-08-07 | bootstrap 接线可行性复核：维持 |
| 2026-08-13 | bootstrap 关停随机序/Ignite 回滚修复 |
| 2026-10-13 | 决策七结论改为删除 bootstrap 与 PhasedStartup（ADR-0112 决策八 #1） |
| 2026-09-20 | Compensation 不在 S_VALIDATE 强制 |
| 2026-09-29 | TrustScorer 改查 `hitl_requests`（ADR-0104 决策五） |
| 2026-10-12 | 订正：账本已删、手工链行数 5805、路径更新 |

## 引用代码

`internal/protocol/saga_compensation.go`、`internal/execute/dag/{executor_node,validator}.go`、`internal/agent/fsm/state_machine_effects.go`、`internal/security/token/capability_token.go`、`internal/action/capability_token.go`、`internal/action/codeact/code_act.go`、`internal/automation/hitl/{gateway,trust_score}.go`、`internal/observability/metrics/record.go`、`internal/execute/orchestrator/{pipeline_compensation,sqlite_blackboard_utils}.go`、`internal/memory/consolidation/{consolidation_background,retrieval_reinforcer}.go`、`internal/bootstrap/`
