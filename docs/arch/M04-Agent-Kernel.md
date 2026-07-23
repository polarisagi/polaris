# 模块 4: Agent Kernel

> 2026-07-12：DAG 执行器（DAGExecutor）与 S_VALIDATE 四层校验管线的物理实现已从
> `internal/agent/dag` 迁出至 `internal/execute/dag`（详见 ADR-0046、
> `internal/execute/CLAUDE.md`）。FSM 通过 `agent/provider.go` 的
> `DAGRunner`/`DAGValidator` 消费端接口驱动，本文档描述的 DAG 语义/校验流程不变，
> 仅物理归属调整；FSM 仍是决定何时进入 S_EXECUTE/S_VALIDATE 的唯一控制流。

> **一句话定位**：Go 状态机持有控制流，LLM（Large Language Model，大语言模型） 仅概率性填空。`[HE-Rule-5]` `[Tier-0-Limit]`
>
> **实现语言**：Go/Rust | **代码位置**：`internal/agent/`（DAG 执行引擎见 `internal/execute/dag/`）
<!-- §跳读: 0-bis:13 职责 / 0-ter:26 不变量速查 / 1:44 状态机 / 2:99 Suspend-on-Idle / 3:115 S_VALIDATE / 4:164 DAG（Directed Acyclic Graph，有向无环图） / 5:246 System1/2 / 6:270 WorldModel / 7:281 推理预算 / 8:340 CrashRecovery / 12:388 已知Bug修复记录 / 13:397 (SOFT)降级 / 14:415 跨模块契约 -->
## 0-bis. 职责边界

| M4 **是** | M4 **不是** |
|-----------|-------------|
| 单 Agent 任务的确定性状态机执行器 | LLM 客户端（那是 M1） |
| System 1/1.5/2 路由决策（基于 SurpriseIndex） | Provider 选择（那是 M1） |
| DAG 规划与并发执行控制 | 跨 Agent 协调（那是 M8） |
| 决定何时调 LLM / 何时调 Tool | 记忆持久化与检索（那是 M5） |
| 崩溃恢复（从 EventLog 回放状态机） | 工具沙箱执行（那是 M7） |
| Prompt 组装（Slot 分离 + Taint 门控） | 技能发现与匹配（那是 M6） |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M4_01 | LLM 仅做结构化填空——Go 状态机持有控制流，禁止 `while True: call LLM` | spec/state.yaml FSM（Finite State Machine，有限状态机） 校验 |
| inv_M4_02 | 重放时不重新调 LLM——用 EventLog 录像值（请求全文 + 响应全文） | M4 §8 ReplayMode 物理切断 |
| inv_M4_03 | PromptFn 为纯函数——同 StateContext → 同 prompt 字节，禁止 wall_clock/random | CI（Continuous Integration，持续集成） `prompt_determinism` 测试 |
| inv_M4_04 | 见下方 [inv_M4_04 补充](#inv_m4_04-补充) | M4 RouteReasoning 代码审计 |
| inv_M4_05 | Suspend-on-Idle——空闲 Agent 不轮询，等待 intent channel 唤醒，空载 CPU<1% | M3 `polaris_goroutines` Gauge |
| inv_M4_06 | 不可逆操作（write_network/privileged）禁止自动回滚——必须显式 HITL（Human-in-the-loop，人机协同） | M7 §5.3 DryRunMode + HITL |

> 进入此模块前必读 **LLM Agent 工程化阵阱和生产原则**：`docs/specs/09-LLM-Agent-Production.md`（HE-Rule-5 实例: A-02/A-09/A-11 + P-1/P-6/P-7）

#### inv_M4_04 补充
System 1 路径零 LLM 调用——`0 < SurpriseIndex < 0.3` 时触发 FastPath（0 表示"未计算"，不触发）；FastPath 在 S_PERCEIVE 合成结果、S_PLAN 旁路 LLM（保留已有 DAGModel 或走空执行路径），不操作 DAGModel/ExecuteResult。

---

## 1. 状态机

状态枚举权威定义见 `internal/protocol/types.go` (AgentState: Idle/Perceive/Plan/Validate/Execute/Reflect/Replan/Rollback/Interrupt/Complete/Failed)。`[HE-Rule-5]` LLM 填空三态输出: TaskModel(S_PERCEIVE) / DAGModel(S_PLAN) / ReflectionModel(S_REFLECT)。

`ReflectionModel` 结构：`{GoalAchieved bool, Errors []string, Learnings []string}`。`onReflectSuccess` 解析后，若 `Learnings` 非空，逐条写入 episodic memory（`sCtx.Mem.Episodic().Append`，EventType="learning"）；写入失败仅 WARN，不阻断状态流转至 S_COMPLETE。

```
S_PERCEIVE ──(LLM_fill 理解任务)──→ S_PLAN ──(LLM_fill 生成 DAG)──→ S_VALIDATE ──┬──OK──→ S_EXECUTE ──┬──OK──→ S_REFLECT ──→ S_COMPLETE
                                                    │                  │              │
                                                    └──Fail─→ S_REPLAN ─┘              └──Fail─→ S_ROLLBACK ──→ S_REPLAN
                                                         ↑                               Saga 逆序补偿           ↑
                                                         └───────────────────────────────────────────────────────┘
                                                    ReplanCount ≥ MaxReplanAttempts: S_REPLAN ──→ S_FAILED ([ESCALATE])

  任意态 ──(UserInterrupt / KillSwitch)──→ S_INTERRUPT ──┬──Resume──→ 原状态
                                                          ├──Redirect─→ S_PLAN (用户修正意图)
                                                          └──Abort────→ S_FAILED
```
5 主执行态: Perceive / Plan / Validate / Execute / Reflect。2 恢复态: Replan / Rollback。1 中断态: Interrupt。1 挂起态: Suspended（Suspend-on-Idle 及 provider_exhausted 挂起）。2 终态: Complete / Failed。加 Idle（空闲等待意图）。共 **12 态**（`state_machine.go` 注册 s_idle ~ s_suspended 共 12 状态、15 条转移）。
ReplanGuard (S_REPLAN 入口): `MaxReplanAttempts` (`spec/state.yaml §m4_kernel.max_replan_attempts`) 超限 → S_FAILED + `[ESCALATE]`

**`[UserInterrupt]` 协议**（inv_global_08, < 200ms 传播）:
- **触发**: M13 `POST /v1/agent/{taskID}/interrupt` (M13 §1.X) → 写 `tasks.interrupt_pending=true` + 通过 EventLog Subscribe 推送至 Agent
- **进入 S_INTERRUPT**: `agent.ContextCancel()` 立即取消 → 所有 LLM call / tool call / [BestOfN] ParallelSampler 子 goroutine 同步终止
- **中断操作语义**:
  - **Resume**: 用户提供"继续"指令 → 恢复原状态 + 注入用户指令到 ZoneImmutable（标记 `source='user_interrupt'`, [TaintLevel]=TaintUserReviewed）
  - **Redirect**: 用户修正任务意图 → 跳转 S_PLAN 重新规划 + 保留原 EventLog（不消耗 ReplanCount）
  - **Abort**: 直接 S_FAILED + Saga 逆序补偿 + workspace GC
- **持久化**: `tasks.suspend_reason='user_interrupt'` + 进入 [Suspend-on-Idle] 等待用户响应
- **SLO**: 触发 → context cancel 完成 < 200ms（与 [KillSwitch] FULLSTOP 同等级）；M3 `polaris_user_interrupt_latency_ms` Histogram 监控

转移表:

| From | Trigger | To |
|------|---------|-----|
| S_PERCEIVE | TriggerPerceiveDone | S_PLAN |
| S_PLAN | TriggerPlanDone | S_VALIDATE |
| S_VALIDATE | TriggerValidateOk | S_EXECUTE |
| S_VALIDATE | TriggerValidateFail | S_REPLAN |
| S_EXECUTE | TriggerExecuteDone | S_REFLECT |
| S_EXECUTE | TriggerExecuteFail | S_ROLLBACK |
| S_REFLECT | TriggerReflectDone | S_COMPLETE |
| S_ROLLBACK | TriggerRollbackDone | S_REPLAN |
| S_REPLAN | TriggerReplanDone | S_PLAN |
| S_REPLAN | TriggerReplanExhausted | S_FAILED |

状态超时:
- S_PLAN: 300s, S_EXECUTE: 600s (计算性状态)
- S_PERCEIVE/S_VALIDATE/S_REFLECT/S_REPLAN: derivedTimeout = upstream_budget - elapsed, 安全地板 30s
- S_IDLE/S_COMPLETE/S_FAILED/S_ROLLBACK: 终端/等待态, 无超时

ReplanGuard 覆盖全部 5 条路径: S_VALIDATE 失败 / S_ROLLBACK 完成 / M1 FatalStreamAbort / M1 JSON Repair 失败 / S_PLAN 拓扑失败。ReplanCount > MaxReplanAttempts → `TriggerReplanExhausted → S_FAILED` → `[ESCALATE]`。S_FAILED 为终态——不进入 S_ROLLBACK，不触发回滚补偿。任务移交 M13 HITL 人工决策。FSM 实现见 `internal/agent/fsm/state_machine.go`（`StateMachine`，唯一生产实现；此前文档提及的 `FallbackFSM`/`use_flowy` build tag 从未落地，2026-07-21 deadcode 审查确认零调用后已删除）。

---

## 2. Suspend-on-Idle Actor

Agent 以 goroutine 形式运行，空闲时挂起释放资源。核心状态机与 FSM 实现见 `internal/agent/`（StateMachine）。

**EpochTracker（上下文指纹隔离）**: `internal/agent/fsm/epoch.go` 实现 `epochTracker`，对每次 LLM 调用前的消息序列计算 SHA-256 全量指纹。指纹变化时 epoch 原子自增，防止陈旧上下文被重复使用。调用方（`state_machine.go`）通过比较 `StateContext.ContextEpoch` 判断上下文是否失效，无需深度比较消息内容。并发安全（`atomic.Int64` + `atomic.Value`）。

Agent 运行循环: 等待 intent channel 上的意图脉冲 → 唤醒推进状态机 → 处理 LLM 和工具返回的 events → 空闲超过 SuspendIdleThreshold (`spec/state.yaml §m4_kernel.suspend_idle_threshold_minutes`) 自动 checkpoint 到 SurrealDB-Core KV（Key-Value，键值） 后释放 goroutine。HITL 等待期间通过 M2 EventLog Subscribe 监听 ApprovalResolved 事件（非 Go channel，防止进程崩溃丢失审批）。

**咨询式挂起（提案）**：`AskHuman` 特权工具扩展本节 Suspend 语义，新增 `SuspendReason=awaiting_user_input`，复用同一 Suspend/Resume 路径处理"信息缺失需澄清"场景（不新增独立错误类型/独立挂起态）。设计草案见 [ADR-0042](./decisions/ADR-0042-hitl-askhuman-consultation.md)（Proposed，未实现）。

**内存持久化失败熔断（GD-13-003，收窄范围后实现）**：四层存储写入路径均为同步落盘，"写失败"以同步 error 直接从 `AppendEpisodicEvent` 等方法返回，不存在原报告描述的"底层 I/O 拒绝导致静默丢失"；真正缺失的只是"收到失败后 FSM 是否转 Suspended"这一层决策。`internal/agent/agent_execute_util.go` 的 `writeEpisodicWithExtract` 捕获写入错误，`isMemoryPersistenceFailure` 用 `apperr.IsCode(err, apperr.CodeStorageUnavailable)` 判定是否为存储层故障（与业务校验错误等不应熔断的错误类型区分）；命中后 `handleMemoryPersistenceFailure` 设置 `StateContext.SuspendReason="memory_persistence_failure"`、写入一条 `m9_storage_degraded` outbox 记录（供后续告警/自愈消费），并调用 `asyncIntent(types.TriggerInterruptReceived)`——复用 FSM `Dispatch()` 中早于 transitions 表检查的全局中断处理器，路由至 `AgentStateInterrupt`（不同于本节 Suspend 语义的 `AgentStateSuspended`，两者是不同的状态/触发对，未新增状态机转换规则）。

内存效率: 活跃 Agent 约消耗 1MB（含 buffer 和栈），休眠 Agent 仅保留约 100 字节的 checkpoint 元数据。Tier 0 硬上限 2 个活跃 Agent。

---

## 3. S_VALIDATE 四层校验

**前置条件**：若 `DAGModel == nil`（FastPath 空执行路径），`executeEffect` 在调用四层校验前直接发出 TriggerValidateOk，不进入校验流程。四层校验仅在 DAGModel 非 nil 时执行。

> **实现说明**：`StateMachine.validateDAG` 和 `executeDAG` 是委托存根，因函数签名限制（无法访问 `a.toolRegistry`）返回硬编码 OK；真正校验逻辑在 `Agent.runValidateDAG`（`dag_validator.go`，L0-L3 四层）和 `Agent.runExecuteDAG`（`dag_executor.go`，Saga 逆序补偿）中执行，由 `Agent.Run` 主循环调用。任何跳过 `Agent.Run` 直接 Dispatch 这两个事件的路径将触发防御守卫返回错误。

| 层级 | 延迟 | 适用范围 | 检查内容 |
|------|------|---------|---------|
| L0 拓扑 | <1ms | 所有 DAG | 见下方 [L0 补充](#l0-补充) |
| L1 确定性 | <1ms | 所有动作 | 见下方 [L1 补充](#l1-补充) |
| L2 启发式 | <5ms | RiskHigh+ | 批量规模（>100）/ 受保护路径 / 资源预估 vs Tier 阈值 |
| L3 LLM 看门狗 | ~200ms | 仅 RiskPrivileged | 见下方 [L3 补充](#l3-补充) |

#### L0 补充
节点数熔断 → DFS 三色环检测 → 深度熔断 → 孤立节点，阈值见 `spec/state.yaml §m4_kernel.plan_dag_max_nodes/max_depth`。

#### L1 补充
TaintGate + JSON Schema 双向校验 + Tool availability + PolicyGate（Cedar-Gate FORBID 优先）。

#### L3 补充
DeepSeek 语义判断输出 ALLOW/DENY；L3 为补充信号，不可推翻 L0/L1/L2 拒绝；LLM 不可用时 fail-open。

> ✅ L3 看门狗已增加 `RiskPrivileged` 过滤，仅对特权任务触发，消除多余延迟。

**ActiveTaintLevel（session 级全局污点）**：已按 DAG 节点 `pCtx.MaxTaintLevel` 传播，L1 TaintGate 对单节点污点拦截有效。


L1.1 资源冲突检测: 规范 artifactID → 对无依赖边的并行写冲突节点自动注入隐式序列化边 (EdgePrecondition), 审计 `implicit_resource_edge`。

**TaintGate** (L1 第一道 — `[Taint-Prop]`):
- **Layer A 上下文传播**: 非系统源 `[Taint-Medium]`+ 数据 → LLM 产出继承上下文最高 `[TaintLevel]` (系统自生成 source∈{compaction, persona_refinement, consolidation, skill_compilation} 排除)
- **Layer A.1 工具调用结构化降级**: 
  - LLM 产出的 DAGNode tool_call 若通过 JSON Schema 校验 (InputSchema + OutputSchema 双向验证)，参数值可经 `[Taint-Sanitizer]` SanitizeBySchema 降一级。
  - 字符串字段仅当 schema 定义 format/pattern/enum/const 内容约束时降级（裸 `{"type":"string"}` 不降级），详见 M11 §2.5 SanitizeBySchema。
  - TaintMedium 工具调用 → TaintLow 参数，允许写入 workspace（解除 RAG（Retrieval-Augmented Generation，检索增强生成）→代码生成链路阻断，同时确保内容不受限的字符串字段不绕过 Taint 防线）。
  - 降级仅在 tool_call schema 校验通过且字段满足内容约束时生效，自由文本 LLM 响应不受此规则影响。每次降级写 `taint_schema_downgrade` 审计事件，标注降级依据。
- **Layer B 精确子串**: `taint_sensitive` 字段 vs active taint set
- **输入反序列化**: TaintedJSONNode 递归树 (禁止 map[string]any, 防 Go JSON 剥离污点标记)
- **TaintBlocked**: → HITL → TaintExemptionToken (field_hash+TTL)
- **SchemaValidator**: Taint 扫描 → InputSchema 校验 → OutputSchema 一致性 → 幂等 ID 合法性（`dag_validator.go` 仅校验结构，字段级 Taint 降级 SanitizeBySchema 在 M7 工具调用层执行）

PolicyGate: `[Cedar-Gate]` {principal, action, resource, context} → FORBID 优先

HeuristicChecker (L2, RiskLevel>=RiskHigh): 批量检查(>100) / 受保护路径(`/etc/`,`/sys/`,`~/.ssh/`→拒绝) / 资源预估 vs Tier 阈值

LLMWatchdog (L3, 仅 RiskPrivileged): 使用 DeepSeek 输出 ALLOW/DENY，不设频次上限（成本可控）。L3 为补充信号——L0/L1/L2 未放行的动作不可因 L3 通过而放行；L3 DENY 推进 S_VALIDATE_FAIL，ALLOW 或 LLM 不可用时 fail-open 推进 S_VALIDATE_OK。实现见 `internal/agent/`。

---

## 4. DAG 数据模型与执行

### 4.1 Micro-DAG vs Macro-DAG

| 维度 | Micro-DAG (M4 职责) | Macro-DAG (M8 职责) |
|------|-------------------|-------------------|
| 节点粒度 | 工具调用 (Tool Call) | 子任务 (Sub-task) |
| 边语义 | 工具间数据依赖 / 时序约束 | 任务间产出/验收依赖 |
| 执行边界 | 单 Agent errgroup 并发 | 多 Agent `[Blackboard]` CAS（Compare-And-Swap，比较并交换） 认领 |
| 生命周期 | Agent Kernel 内部，不发布 | M8 发布到 `[Blackboard]` |
| 所有权 | M4 独占 | M8 编排 / M4 独立执行 |
| Context 隔离 | 共享父 Agent PromptBuilder | 见下方 [Context 隔离补充](#context-隔离补充) |

#### Context 隔离补充
**每个子 Agent 持有独立 PromptBuilder 实例与 context window**，仅通过 Blackboard 结构化 result entry 交换（[Sub-agent-Isolation]）。

> Sub-agent 物理隔离：M8 派发 Macro-DAG 节点时为每个执行 Agent 创建独立 `kernel.PromptBuilder` 实例（`internal/agent/`），禁止共享父 Agent 内存中的 ImmutableCore/MutableSkill/TaintedData zone。子任务结果以结构化 schema（M8 5 原语之 Result）写入 Blackboard，父 Agent 通过订阅 Blackboard 事件消费，避免上下文污染与 token 膨胀（见 00-Global-Dictionary §9-bis [Sub-agent-Isolation]）。

### 4.2 数据模型

DAGNode/DAGEdge/EdgePolarity/RetryPolicy/Compensation 类型定义见 `internal/agent/`（旧版 `internal/agent/` 顶层文件已标记 Deprecated）。

**DAGModel 两种产出来源，收敛为同一表示**：S_PLAN 阶段 LLM 产出工具调用有两条输入形态——① 文本 JSON DSL（LLM 直接输出符合 plan_dag Schema 的 JSON 文本）；② Provider 原生 `tool_calls` 结构化字段（`types.WithTools` 随工具目录非空自动附加，`doStreamInfer` 消费 adapter 侧 `StreamToolCall` 流式事件累积为 `ProviderResponse.ToolCalls`，经 `toolCallsToDAGJSON` 转换为等价的 DAGModel JSON）。两条输入形态在此收敛为同一个 DAGModel，下游 S_VALIDATE 四层校验与 DAG Executor 不感知来源差异，不存在第二条并行执行路径（2026-07-14，见 §12 Bug 记录 + `internal/agent/agent_execute_effect.go`）。

### 4.3 DAG Executor

DAGExecutor 实现见 `internal/agent/`（旧版 `internal/agent/` 顶层文件已标记 Deprecated）。执行流程:
0. 调用 M8 Blackboard.BeginExecution(taskID, agentID): CAS Claimed→Executing（首次工具调用前的状态转移，闭合 Pending→Claimed→Executing→Done/Failed 完整生命周期）
1. findReadyNodes: DependsOn ⊆ completedSet → 就绪，同批字典序优先
2. 副作用分类: read_only/pure → 并发; write_local/write_network → 必须声明 CompensationAction
3. 启动 LeaseHeartbeat goroutine: 每 15s(±5s jitter) 续期，防 M8 Reaper 误判超时
4. errgroup 并发执行，sem channel 限制并发度 (`spec/state.yaml §m4_kernel.max_concurrent_nodes`)
5. 任意失败 → 已完成并行节点逆序 Undo 补偿
6. 循环至全部完成 → 停止 Heartbeat

### 4.4 Dynamic DAG Replanning

节点输出 `[SurpriseIndex]` >0.7 → 未执行下游子图局部重规划。已成功节点保留(防双重副作用)。若必须覆盖已执行节点: 先 Saga Compensation 成功才加入 replan。重规划在 S_EXECUTE 内部, 不跨状态机边界。

### 4.5 StepScorer + Adaptive Max-Steps

实现见 `internal/agent/`（StepScorer，同包调用零 import 开销）。

- **Tier 0 (纯静态启发式)**: 权重 toolSuccess=0.4, schemaCheck=0.3, latency=0.2, tokenEfficiency=0.1。Score 从 1.0 起点按四项扣分，latency/token 惩罚 cap 封顶。
- **Tier 1+ (启发式 + 1.5B 挂载 PRM（Process Reward Model，过程奖励模型） 融合)**: M1 LocalProvider 加载极小 PRM，对中间步语义打分 (+1,0,-1)，融合权重 0.6。PRM 超时 >100ms 或 OOM（Out of Memory，内存溢出） → 安全降级纯静态。

**当前实现状态：已实现（2026-07-03）**。实现见 `internal/agent/step_scorer_prm.go`。

与文档描述的差异点（系统级复用而非另起炉灶，理由见文件头注释）：
- 不新增独立的 PRM 模型加载/常驻通道——`rust/substrate/src/llama_infer`（P3-1）单槽位覆盖式设计下同时常驻两个模型需要更大的 FFI 协议改造，超出本次范围。
- 转而复用 `Agent.provider`：若其恰好实现 `protocol.LocalProvider`（`newStepScorer()` 在 `Agent` 构造时做类型断言 + `probe.GlobalFeatureGate().HardwareTier() >= Tier1` 判定，两者皆满足才挂载 `stepScorer.prm`，否则等价于纯静态 Tier0 路径）。
- 并复用 `LocalAdapter.Infer()` 已支持的 GBNF（GGML BNF，GGML 巴科斯范式） grammar 约束（`root ::= "+1" | "0" | "-1"`）做三态离散打分，避免自由生成拖慢单步预算。
- `scoreWithPRM()` 融合公式：`(1-0.6)*staticScore + 0.6*prmNormalized`（+1→1.0/0→0.5/-1→0.0），100ms 硬超时（`context.WithTimeout`，独立于调用方 ctx）。
- FFI 错误（含 Rust 侧 `catch_unwind` 转换的 OOM/panic）与非法输出均安全降级为纯静态分，不向上抛出 error。
- 调用点见 `internal/agent/agent_execute.go`（工具调用后打分处）。单元测试见 `internal/agent/step_scorer_prm_test.go`（覆盖融合方向、超时降级、错误降级、非法输出降级、Tier0/远程 Provider 不启用）。

**Adaptive Max-Steps 闭环**:
- `StateContext` 持有 `StepsUsed / MaxStepsLimit`；`AgentConfig.MaxSteps` 在首次 `Run()` 时写入 `MaxStepsLimit`（0=无上限，不推荐生产）。
- `Agent.Run()` 每轮 trigger 前计步：`StepsUsed > MaxStepsLimit` → FSM 熔断至 `S_FAILED`，错误码 `MAX_STEPS_EXCEEDED`。
- 每次工具调用后调用 `adjustMaxSteps(current, score)`：score < 0.5 → 收紧 10%（防低质量循环），score ≥ 0.5 → 保持不变（防预算膨胀）。**✅ 已修复**：`StateContext.InitialMaxStepsLimit` 记录初始值，每次 S_PLAN 生成新 DAG 时重置 `MaxStepsLimit`，惩罚仅在当前 DAG 执行周期内生效，不跨任务累积。

**Best-of-N 与 Replanning 阻断**:
双路径输出为 Best-of-N 搜索提供置信度排序，低分分支标记为 MEMF（Memory of Errors and Mistakes Framework，错误记忆框架） 失败候选池，累积低于警戒线立即触发重规划或 Saga 补偿。

### 4.6 ProcessRewardModel — S_PLAN 候选 DAG 选优

ProcessRewardModel（PRM）集成于 Agent Kernel（`internal/agent/`），在 **S_PLAN 阶段**对多个候选 DAG 方案并发打分，选出最优规划后进入执行。与 §4.5 StepScorer 职责不同：StepScorer 对执行中单步实时评分，PRM 面向规划阶段的完整候选集。

**触发条件**：任务复杂度超过可配置阈值（默认 0.5）且 PRM 已启用；低于阈值的简单任务直接跳过，零额外 token 消耗。复杂度由 S_PERCEIVE 阶段写入任务上下文。

**多候选并发选优流程**：S_PLAN 阶段并发生成多个候选 DAG（默认 3 个），再并发调用 budget-tier LLM 对每个候选打分（0–1），选出最高分方案推进至 S_VALIDATE。若全部候选分数低于最低可接受阈值（默认 0.4），fallback 取第一个候选，保证规划不丢失。

**关键配置**（均可由 M8 Orchestrator 在运行时注入覆盖）：默认关闭须显式开启；打分模型使用 budget-tier LLM；候选数默认 3（研究数据显示 3 候选 ROI 最优）；复杂度门限默认 0.5。

**DAG 执行结果聚合**：S_EXECUTE 完成后统一收集所有节点输出。单节点直接取 output；多节点序列化为结构化结果集，确保 S_REFLECT 阶段可访问完整 DAG 执行上下文。

### 4.7 spawn_planner — 后台子规划器

S_PLAN 阶段若任务复杂度触发子规划策略，异步启动规划器池（`internal/swarm/planner/`）。规划器池内置多 Worker 策略引擎，通过内部通道将子计划异步回传给父 Agent。

---

## 5. System 1/2 双轨路由

`[HE-Rule-5]` System 1 物理边界: 零 LLM 调用, 100% 本地 Python/Go 技能（ContainerSandbox）+ SurrealDB-Core KV 缓存。未命中 → 无条件升级 System 1.5。FastPath 跳过 LLM 但仍执行确定性记忆写入（Episodic + Outbox），不违反零 LLM 约束。

| 路径 | `[SurpriseIndex]` | 延迟 | 模型来源 |
|------|-------------------|------|---------|
| System 1 | <0.3 | 亚毫秒 | L0 技能缓存 (零 LLM) |
| System 1.5 | 0.3-0.6 | 毫秒-秒 | M1 Budget Pool |
| System 2 | ≥0.6 | 秒级 | M1 Reasoning Pool |

**`SelectThinkingMode` 注入**（与 System 路由正交）: M4 `transitions.go` 在 LLM 调用前调用 `SelectThinkingMode(SurpriseIndex, replanCount, TaintLevel)` 决定三档 `[ThinkingMode]`（Disabled / High / Max），通过 `protocol.WithThinkingMode(mode)` 作为 `InferOption` 传入 Adapter。档位触发条件与 Provider API（Application Programming Interface，应用程序接口） 映射见 M1 §5.2-bis。

**SurpriseIndex 计算来源（ADR-0025（Architecture Decision Record，架构决策记录） BUG-D）**：`populateSessionContext` 优先从注入的 `SurpriseReader`（`learning/surprise.SurpriseCalculator`，三分量：Jaccard + MEMF + Markov）读取；未注入时退回 `ComputeBasic`（仅 Jaccard）。计算结果同步写入 `metrics.GlobalSurpriseIndex`，供 `SelectThinkingMode` 消费。`SurpriseReader` 为 consumer-side 接口，防 L1→L2 包循环。

RouteReasoning:
0. si = `Agent.surpriseCalc.CurrentSurprise()`（已注入时）或 `metrics.GlobalSurpriseIndex().ComputeBasic(nil, toolSeq)`（退化路径）→ 两者均不可用 → 0.5。**`si=0` 为默认零值，不触发 FastPath；正式 FastPath 仅在 `0 < si < 0.3` 时激活。**
1. `0 < si < 0.3` → FastPath：合成 S_PERCEIVE 结果跳过 LLM，S_PLAN 阶段同样旁路 LLM（保留已有 DAGModel 或走空执行路径）。skillCache 命中直接执行 Wasm; 不兼容 fall through
2. 未命中或 si>=0.3 → 调用 `M6.SkillSelector.SelectTopK(intent, K=5)` 选取候选工具/技能描述（**Tool Selection > Tool Design**：避免把全部工具列表塞给 LLM 导致选择崩溃）→ buildMessages → `providerRouter.Route`
3. buildMessages: ImmutableCore + GoalDescription + DAG 上下文 + SkillSelector 选取的 top-K 工具描述

**AgentPool（ADR-0029 §E）**：`ChatHandler` 持有 `AgentPool`（consumer-side 接口）而非单个 `AgentController`。每个 sessionID 对应独立 `Agent` 实例，容量由 `TierParams.MaxConcurrentAgents` 限制（Tier-0: 4）；超容量时 Acquire 等待 100ms 后返回 `CodeResourceExhausted`。Idle 超过 10 分钟的 session 由 `Pool.GC()` 低频回收。每个新建 Agent 实例由 `Pool.Acquire` 立即以其自身生命周期 ctx 启动常驻 `Run()` 事件循环（消费其内部 intent channel），语义与 Supervisor 对单例 `agent-0` 的启动方式一致；`GC()` 回收 idle session 时对应调用 `Shutdown()` 停止该循环（详见 ADR-0029 Addendum，2026-07-12 复核修复：此前该循环从未启动，per-session 会话 FSM 实际不会推进状态）。

---

## 6. World Model

L1 快速路径：SurpriseIndex < 0.3 时旁路 LLM（在 `agent_execute.go` FastPath 分支执行）。L2：LLM 执行后，SurpriseIndex 更新并影响下一轮路由。StatePredictor / Isotonic Regression 已废弃，路由完全由 SurpriseIndex 主导。

**知识空缺感知 (Knowledge Gap Awareness)**:
LLM 推理前调用 `WorldModel.AssessGrounding` 评估上下文充分性。上下文不足时将警告注入 prompt 末尾（`[System Warning: Knowledge gap detected. Consider further retrieval...]`），引导 Agent 优先检索，不直接拦截执行。

WorldModel 实现见 `internal/memory/`；上下文组装由 `internal/agent/` 的 `PromptBuilder` 统一执行（含四 Zone 布局 + TaintData Spotlighting 门控）。

---

## 7. 推理预算管理

四层预算:

| 层级 | 粒度 | 机制 | 默认值 |
|------|------|------|--------|
| 思考步数 | 单次 DAG 推理步数 | MaxReasoningSteps | 5 |
| 思考 token | 单次 LLM reasoning | MaxThinkingTokens | 4096 |
| 任务预算 | 单次 Agent 任务 | TaskTokenBudget | 50K |
| Session 预算 | 单次 Session | SessionTokenBudget | 200K |

三模式: `fixed` (MaxReasoningSteps=5, MaxThinkingTokens=4096) / `adaptive` (`min(16384, 4096×(1+[SurpriseIndex]×3))`, 1000+ 样本后) / `batch` (32K, 夜间)

> **BudgetManager**（`budget.go`）：
> - **入口**：BudgetManager 是唯一预算判断入口（Task 11）。实现 `fsm.BudgetController` 接口，通过 `SetBudget()` 注入 `StateContext.Budget`。
> - **记账**：LLM 推理成功后调用 `ConsumeTokens(actualTokens)` 精确记账；`EstimatedSpendUSD()` 向 Cedar `budget_cap` 规则填充 `monthly_spend_usd`/`monthly_budget_usd`。内联 `StateContext.TokenBudget/TokensUsed` 逻辑保留作向后兼容，`Budget != nil` 时双轨并行（均计账）。
> - **持久化与更新**：`protocol.AgentController` 提供 `SetMonthlyBudgetUSD(float64)`；`boot_server.go` 启动期从 `BudgetRepository.GetBudget()` 读取持久化值注入 Agent（避免启动时预算被重置为 0）；`HandleSetBudget`（`internal/gateway/server/sysadmin/budget.go`）持久化 `kv_store` 成功后同步调用 `Agent.SetMonthlyBudgetUSD()` 热更新。
> - **闭环**：写路径与运行时读路径保持一致，二者与 `EstimatedSpendUSD()` 共同构成完整闭环。

### 7.0 任务级预算自适应截断

任务级 Token 预算在每次 DAG 节点循环入口处执行三级检测，实现见 `internal/agent/`：

| 阈值 | 条件 | 动作 |
|------|------|------|
| 50%（警告） | `TokensUsed * 100 / TokenBudget >= 50` | 首次写 `BudgetWarned=true`，记录 `slog.Info`；后续循环不重复 |
| 75%（压迫） | `TokensUsed * 100 / TokenBudget >= 75` | 首次写 `BudgetPressure=true`，记录 `slog.Warn`；触发 S_PLAN `[BUDGET_CONSTRAINT]` 指令注入 |
| 100%（硬熔断） | `TokensUsed > TokenBudget` | FSM 直接转 `S_FAILED`，错误码 `INFERENCE_OOM` |

**BudgetPressure → [BUDGET_CONSTRAINT] 注入**（`promptPlan()`，`state_machine.go`）：
- `BudgetPressure=true` 时，在 S_PLAN Prompt 末尾追加系统指令：
  `[BUDGET_CONSTRAINT] Token budget > 75%. Generate a MINIMAL DAG: max 3 nodes, only strictly necessary tool calls, no exploratory steps. Remaining: N tokens.`
- 该指令来源标记 `TaintNone`，经 `SanitizeToSafe` 后注入 `WriteInstruction` slot（符合 Taint 类型约束）。
- `BudgetWarned`/`BudgetPressure` 字段定义在 `StateContext`；重放时从 EventLog 恢复，保证 `[inv_M4_03]` promptFn 确定性。

M4 不重复实现 TokenBurnRate 检测逻辑，也不独立触发 KillSwitch 阶段变迁。TokenBurnRate 的 CANONICAL SOURCE 是 M3（EMA_5s + EMA_30s），M3 将速率直接推送至 M11 KillSwitch.CheckAndAct（M11 §4.3），这是触发 KillSwitch 阶段变迁的**唯一路径**。M4 在每次 LLMFillEffect 前读取 `observability.GlobalKillswitchStage`（原子 int32）并响应（`agent_execute.go §1.5`）:

- **Throttle 阶段**（已实现）: `sCtx.MaxStepsLimit` 收紧至 3，`sCtx.ThrottleNoNetwork = true`；DAGExecutor `toolExecFn` 对含 `SideNetworkCall` 副作用的工具返回 `ErrForbidden`
- **Pause 阶段**（已实现）: 返回 CodeInternal 错误，挂起当前 LLM 调用，等待 M11 恢复或 ESCALATE
- **Fullstop 阶段**（已实现）: 状态机推进 S_FAILED，拒绝新 Infer 调用

跨模块交互规则见 `00-Global-Dictionary.md` [XR-01]。

ContextWindowManager（热路径上下文管理，与 M5 SessionCompressor 冷路径协同）:
- maxTokens=90000
- >70% → salience 排序，底 30% 候选交由 M5 SessionCompressor 压缩（M5 §11，LLM 锚定迭代总结）
- >90% → 语义结构感知逐出（以完整 DAGNode/tool_result/Episodic Event 为单位）
- 仍超限 → 触发 M5 Consolidation 全量压缩（M5 §9，跨 Session 语义压缩）

M4 仅持有热路径上下文窗口管理与触发判断；具体压缩算法、锚定策略、cold path 实现委托给 M5（Compaction as First-Class，单一权威源）。

### 7.1 `[ReasoningState]` 跨轮持久化

`StateContext.LastReasoningContent` 持有本轮 LLM 返回的 `reasoning_content`（由 Adapter 从响应中提取写入 `ProviderResponse.ReasoningContent`，M4 在 `agent_execute.go` 存入 `sCtx`）。下次 LLM 调用构建 messages 时，将其作为 assistant 消息的 `reasoning_content` 字段回传，满足 DeepSeek V4 Pro 多轮工具调用的 API 约束。

跨 session / 跨任务不继承。`FeatureReasoningStateCarry`（Tier 1+ 启用）扩展此行为至 episodic_events 落盘持久化（msgpack + AES-256-GCM，SessionPIIVault.SecureZero 同步清零）。

---

## 8. Crash Recovery

满足 [HE-Rule-6]（State-in-DB（Database，数据库））——崩溃恢复从 M2 EventLog 回放，不依赖独立的 FSM checkpoint 文件。状态持久化时机：`runFSM` 在每次状态转移完成后显式触发写入（见 `internal/agent/fsm/transitions.go`），Transition.Effects 回调本身不负责持久化。SurrealDB-Core KV 的 goroutine checkpoint（§8.1）仅用于空闲时释放 goroutine 栈以节省内存，非崩溃恢复路径。

**回放机制**: M4、M5、M11 启动时统一检查 [ReplayMode] 标志（进程级 atomic.Bool）。回放期间禁止所有外部副作用（EmitEvent/ToolCall/Outbox）——纯函数式重建内存状态。追平事件流后退出回放模式，从崩溃点继续执行。

**网络抖动恢复**:
触发: 长任务 (>5min) 每 5min 或每 10K tokens 推理输出 → SnapshotContext
写入: SurrealDB-Core KV `session_snapshots` namespace，key=`{session_id}:{seq}`，TTL=24h，per session 上限 5
events 表: 轻量 `source='snapshot_checkpoint'` 记录（含 SurrealDB-Core key 引用），供时序定位
PII（Personally Identifiable Information，个人敏感信息）: 快照不含明文——ToolResult 经 M7 §4.3 Step 5 PostExecution Redact 后写 EventLog；FSM Snapshot 保留原始值供同 session 崩溃恢复
恢复: 优先加载最近快照 → 差量回放后续事件；快照损坏 → 回退 EventLog 全量回放（ToolResult 红化版本，需 SessionPIIVault 仍存活解析 token）
与 M5 SessionResume 共享同一 barrier 协同重建。

**双重幂等防线**: 第一层 isReplaying 标志 物理切断副作用；第二层 UNIQUE(session_id, seq) 约束 + idempotency_key 保证重复事件的幂等消费。

**Replay Key 算法（录像 key）**: 所有写入 EventLog 的事件 ID 格式为 `{session_id}:{seq}:{event_type}`，seq 为单调递增计数器，保证同 session+seq 始终生成相同 ID，不依赖 wall clock。实现见 `internal/agent/`。回放时若 event_type+ID 与 FSM PromptFn 占位符对应关系缺失，触发 `g_inv_08` 防护（禁止重新调 LLM，进入 REPLAY_MISMATCH 审计）。

在非重放路径上（`uuid.New().String()` 生成 2PC 中间事件），同一事件的重放时间戳不同但通过 idempotency_key 防重入——回放时 `isReplaying=true` 物理切断所有副作用，确保这些 UUID 事件不会被重新投递。

**Snapshot 策略**: 步频与保留数见 `spec/state.yaml §m4_kernel.snapshot_interval_steps` / `snapshot_retention_count`。Snapshot 损坏时回退到完整 EventLog 回放。

**S_REPLAN 降级**: M1 CircuitBreaker 熔断时，执行零 LLM 的确定性图剪枝（纯 Go 图遍历）：移除失败节点及其所有直接后继节点，注入 degraded_replan 标记。此步骤禁止任何 LLM 调用——剪枝逻辑为纯函数，幂等且可重放。

**S_REPLAN 扩展激活降级**（ADR-0065，与上述 CircuitBreaker 剪枝为不同触发路径）：按需扩展激活（`FindAndActivate`）超时或失败时不再无重试静默放行，改为有限重试（预算封顶于 `replanExtActivationTimeout`，默认 3s）；重试仍耗尽 → 会话上下文置 `ReplanExtActivationDegraded` 标记 + `GlobalReplanExtActivationDegradedTotal` 指标计数，随后仍照常 dispatch `TriggerReplanDone`（不新增 FSM 状态、不阻塞状态机主流程）。下游 DAG 执行遇 `capability_gap`（工具确实缺失）时，若该标记为真则直接快速失败并给出明确原因，取代此前"缺工具仍强行重规划、大概率再次落空"的空转路径。

**`ErrAllProvidersFailed` 专项处理（全 Provider 熔断）**:
1. **确定性图剪枝后检查剩余 DAG 节点**:
   - (a) 有 System 1 可执行节点（SurpriseIndex <0.3，零 LLM，纯本地 Python/Go 技能）→ 继续执行，**不消耗 ReplanCount**；LLM 依赖节点等 Provider 恢复。
   - (b) 全部需 LLM → **不消耗 ReplanCount**，转 `Suspended(suspend_reason=provider_exhausted, provider_suspended_count++)`；Blackboard 写标记；调 `SessionPIIVault.SuspendSnapshot(ctx, taskID)` 持久化 PII。
2. `provider_suspended_count > 5` → 终止自动唤醒，触发 `[ESCALATE]` + HITL。
3. 剪枝后剩余 DAG 为空 → `[ESCALATE]` 人工审批。

**Provider 恢复唤醒**: 
M1 CircuitBreaker Open→Closed (§7.3) → M2 Outbox 投递 `target_engine:"m4_provider_recovery"` 事件。Handler 注册于 M2 全局 Outbox Worker（`internal/store/`），实现位于 `internal/agent/`——不在 M4 内独立 Worker（违反 M2 §2.3 单写者）。执行序列:
  1. 扫描 M8 Blackboard 全部 `suspend_reason=provider_exhausted` 任务。
  2. 逐一 `M11.SessionPIIVault.RestoreFromSnapshot(ctx, taskID)` 解密恢复 PII token。
  3. `M8.Blackboard.ResumeFromSuspended(taskID)` 重置 Suspended→Pending。
  4. 重新调度（M8 ListenLoop 扫描认领）。

**FSM 终态 PII 清零**: M4 转 S_FAILED / S_COMPLETE 时，先于 WorkspaceManager GC 调 `SessionPIIVault.SecureZero(ctx, taskID)`，pii_vault_blob 先于 workspace 删除（GDPR 主动擦除）。无可执行节点 → `[ESCALATE]`。

**Saga 补偿**: 确定性函数 + 预定义 HTTP（HyperText Transfer Protocol，超文本传输协议） 模板，禁止 LLM 参与。补偿前 M11 PolicyGate.Review 预检——FORBID → `[ESCALATE]` + `compensation_blocked_by_policy_revocation` 审计。非权限型失败重试 3 次（exponential backoff）。

完整时序见 `DIAGRAMS.md#eventlog`。

---

## 12. 已知 Bug 修复记录

| 级别 | 文件 | 函数 | 问题描述 | 修复 | Commit |
|------|------|------|---------|------|--------|
| P1 | `internal/memory/` (synaptic_plasticity) | 衰减指数计算 | 自定义幂函数将指数截断为整数，导致近期访问边被过度修剪 | 改用标准库 `math.Pow` | 4d7682f |
| P1 | `internal/agent/agent_execute_effect.go` | doStreamInfer | `InferOptions.Tools`（`types.WithTools`）全仓零消费——各 Provider adapter 的原生 `tool_calls` 流式事件（`StreamToolCall`，`stream.go`/`anthropic_request.go`/`google_request.go`）早已产出，唯独消费端从未接收，S_PLAN 只能靠 LLM 输出文本 JSON DSL 一条路径产出 DAGModel | 新增 StreamToolCall 分支累积 `ProviderResponse.ToolCalls`；`toolCallsToDAGJSON` 把原生 tool_calls 转换为 plan_dag Schema 期望的 DAGModel JSON——与文本 JSON DSL 路径在此收敛为同一 DAGModel 表示，复用既有 S_VALIDATE 四层校验 + DAG Executor，不新建第二条执行路径 | b99b9cf |

---

## 13. 降级与失败模式（5 问全覆盖）

| 故障 | (Q1) 检测 | (Q2) 影响范围 | (Q3) 即时反应 | (Q4) 自动恢复 | (Q5) 人工介入触发 |
|------|----------|------------|------------|------------|----------------|
| LLM Fill 多次重试失败 | retry counter ≥ MaxRetry | 单 Agent 当前状态转移 | OnFailure callback → s_error | 部分（下层 M1 CB 恢复后重试） | s_error 进 audit |
| DAG 节点执行失败（可逆） | tool error | 单 step | step retry with backoff → 仍失败 → Saga 逆序补偿 → s_rollback | 是 | — |
| DAG 节点执行失败（不可逆） | Reversible=false + error | 单 Agent | s_failed + HITL 告警 | 否 | 必须 HITL |
| StructOutput JSON 解析失败 | JSON Repair 失败 | 单次 LLM 调用 | retry (1 次) → 仍失败 → s_replan | 是 | 同模型连续 ≥10 次 → audit |
| ReplanGuard 超限 | ReplanCount > MaxReplanAttempts (`§m4_kernel.max_replan_attempts`) | 单 Agent | s_failed + HITL 告警 | 否 | 必须 HITL |
| DAG 死锁（无就绪节点） | findReadyNodes 返回空且未完成节点 > 0 | 单 Agent | ErrDAGDeadlock → s_error + EventLog | 否 | M12 复盘 |
| Agent goroutine panic | recover() | 单 Agent | Supervisor OneForOne 自动重启 + EventLog 回放 | 是 (100ms→30s, 5 次上限) | 同 Agent 反复 panic ≥3/min → escalate |
| HITL 审批超时 | deadline 到期 | 单 Agent | s_rollback（不触发 KillSwitch，仅当前任务失败） | 用户重新发起 | 反复 expire → audit |
| 进程崩溃 | exit | 全局 | 重启后从 EventLog 重放，不重调 LLM | 是 | — |
| Replay key 漂移 | hash mismatch | 单会话重放 | 走 LLM 重新调用 + audit | 是 | 频繁漂移 → M12 排查 |

与 OSMemoryGuard 协同: ResourceGovernor.Admit() 在 Agent 启动前校验可用内存 + CPU。OSMemoryGuard 触发 L3 临界 → 仅保留当前执行中的 Agent，禁止新唤醒。


## 14. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces.go` + `types.go`。本表仅列依赖方向 + 一句话语义 + 锚点。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M4→M1 | Provider.Infer / StreamInfer | LLM 推理；SurpriseIndex consumer。M1 §2, §4 |
| M4→M2 | EventLog Append / GetEvents | 崩溃恢复回放真相源。M2 §2.1 |
| M4→M2 | Outbox `m4_provider_recovery` handler | `internal/store/` 注册；实现 `internal/agent/`。M2 §2.5, M4 §8 |
| M4→M3 | OTel（OpenTelemetry） spans + SurpriseIndex 消费 | 双层回退 完整版→基础版→0.5。`[HE-Rule-1]` M3 §4 |
| M4→M5 | PromptBuilder / HybridRetriever | 记忆检索 + 上下文组装（PromptBuilder 四 Zone 布局）。M5 §2, §7 |
| M4→M6 | SkillLookup / SkillRegister | System 1 技能缓存 + Persona 兼容性。M6 §3, §4.3 |
| M4→M7 | ToolRegistry.ExecuteTool | S_EXECUTE 节点调用 `[Wasm-Sandbox]`。M7 §3 |
| M4→M11 | TaintGate / PolicyGate / KillSwitch | 查阅，仅响应不主动触发。M11 §2, §4 |
| M4→M11 | SessionPIIVault | Suspend 时落 pii_vault_blob；Restore/SecureZero 跟随 FSM 终态。M11 §5.1 |
| M8→M4 | Blackboard.CAS Claim / LeaseHeartbeat | 多 Agent 调度入口。M8 §1 |
| Schema | AgentState 枚举、DDL（Data Definition Language，数据定义语言） 001_events / 003_episodic_memory / 007_tasks（含 pii_vault_blob / suspend_reason / provider_suspended_count）| `internal/protocol/types.go`, `internal/protocol/schema/` |
| 全局字典 | HE-Rule-5 状态机控制流、XR-01 | 00-Global-Dictionary §1-bis, §1-ter |
| 时序图 | EventLog 回放、KillSwitch 响应链 | DIAGRAMS.md#eventlog, #killswitch |

`[Module-Topology]` `[Code-Package-Mapping]`

---

## 15. 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m4_kernel`。
