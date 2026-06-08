# 模块 4: Agent Kernel

> M4, `pkg/cognition/` | Go 状态机持有控制流，LLM 仅概率性填空 | `[HE-Rule-5]` `[Tier-0-Limit]`
> **§跳读**: 0-bis:5 职责 / 0-ter:18 不变量速查 / 1:31 状态机 / 2:84 Suspend-on-Idle / 3:94 S_VALIDATE / 4:125 DAG / 5:209 System1/2 / 6:231 WorldModel / 7:243 推理预算 / 8:291 CrashRecovery / 12:348 (SOFT)降级 / 13:366 跨模块契约
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
| inv_M4_01 | LLM 仅做结构化填空——Go 状态机持有控制流，禁止 `while True: call LLM` | spec/state.yaml FSM 校验 |
| inv_M4_02 | 重放时不重新调 LLM——用 EventLog 录像值（请求全文 + 响应全文） | M4 §8 ReplayMode 物理切断 |
| inv_M4_03 | PromptFn 为纯函数——同 StateContext → 同 prompt 字节，禁止 wall_clock/random | CI `prompt_determinism` 测试 |
| inv_M4_04 | System 1 路径零 LLM 调用——`0 < SurpriseIndex < 0.3` 时触发 FastPath（0 表示"未计算"，不触发）；FastPath 仅合成 S_PERCEIVE 结果，不操作 DAGModel/ExecuteResult | M4 RouteReasoning 代码审计 |
| inv_M4_05 | Suspend-on-Idle——空闲 Agent 不轮询，等待 intent channel 唤醒，空载 CPU<1% | M3 `polaris_goroutines` Gauge |
| inv_M4_06 | 不可逆操作（write_network/privileged）禁止自动回滚——必须显式 HITL | M7 §5.3 DryRunMode + HITL |

---

## 1. 状态机

状态枚举权威定义见 `internal/protocol/types.go` (AgentState: Idle/Perceive/Plan/Validate/Execute/Reflect/Replan/Rollback/Interrupt/Complete/Failed)。`[HE-Rule-5]` LLM 填空三态输出: TaskModel(S_PERCEIVE) / DAGModel(S_PLAN) / ReflectionModel(S_REFLECT)。

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
5 主执行态: Perceive / Plan / Validate / Execute / Reflect。2 恢复态: Replan / Rollback。1 中断态: Interrupt。2 终态: Complete / Failed。加 Idle（空闲等待意图）。共 11 态。
ReplanGuard (S_REPLAN 入口): `MaxReplanAttempts` (`spec/state.yaml §m4_kernel.max_replan_attempts`) 超限 → S_FAILED + `[ESCALATE]`

**`[UserInterrupt]` 协议**（inv_global_08, < 200ms 传播）:
触发: M13 `POST /v1/agent/{taskID}/interrupt` (M13 §1.X) → 写 `tasks.interrupt_pending=true` + 通过 EventLog Subscribe 推送至 Agent
进入 S_INTERRUPT: agent.ContextCancel() 立即取消 → 所有 LLM call / tool call / [BestOfN] ParallelSampler 子 goroutine 同步终止
中断操作语义:
  - **Resume**: 用户提供"继续"指令 → 恢复原状态 + 注入用户指令到 ZoneImmutable（标记 `source='user_interrupt'`, [TaintLevel]=TaintUserReviewed）
  - **Redirect**: 用户修正任务意图 → 跳转 S_PLAN 重新规划 + 保留原 EventLog（不消耗 ReplanCount）
  - **Abort**: 直接 S_FAILED + Saga 逆序补偿 + workspace GC
持久化: `tasks.suspend_reason='user_interrupt'` + 进入 [Suspend-on-Idle] 等待用户响应
SLO: 触发 → context cancel 完成 < 200ms（与 [KillSwitch] FULLSTOP 同等级）；M3 `polaris_user_interrupt_latency_ms` Histogram 监控

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

ReplanGuard 覆盖全部 5 条路径: S_VALIDATE 失败 / S_ROLLBACK 完成 / M1 FatalStreamAbort / M1 JSON Repair 失败 / S_PLAN 拓扑失败。ReplanCount > MaxReplanAttempts → `TriggerReplanExhausted → S_FAILED` → `[ESCALATE]`。S_FAILED 为终态——不进入 S_ROLLBACK，不触发回滚补偿。任务移交 M13 HITL 人工决策。FSM 实现见 `pkg/cognition/fsm.go:FallbackFSM`；`go build -tags use_flowy` 启用 flowy 增强版。

---

## 2. Suspend-on-Idle Actor

Agent 以 goroutine 形式运行，空闲时挂起释放资源。核心结构见 `pkg/cognition/kernel/state_machine.go:StateMachine`，FSM 实现见 `pkg/cognition/fsm.go:FallbackFSM`。

Agent 运行循环: 等待 intent channel 上的意图脉冲 → 唤醒推进状态机 → 处理 LLM 和工具返回的 events → 空闲超过 SuspendIdleThreshold (`spec/state.yaml §m4_kernel.suspend_idle_threshold_minutes`) 自动 checkpoint 到 SurrealDB-Core KV 后释放 goroutine。HITL 等待期间通过 M2 EventLog Subscribe 监听 ApprovalResolved 事件（非 Go channel，防止进程崩溃丢失审批）。

内存效率: 活跃 Agent 约消耗 1MB（含 buffer 和栈），休眠 Agent 仅保留约 100 字节的 checkpoint 元数据。Tier 0 硬上限 2 个活跃 Agent。

---

## 3. S_VALIDATE 四层校验

**前置条件**：若 `DAGModel == nil`（FastPath 空执行路径），`executeEffect` 在调用四层校验前直接发出 TriggerValidateOk，不进入校验流程。四层校验仅在 DAGModel 非 nil 时执行。

```
L0 拓扑 (<1ms, 所有 DAG): 节点熔断(`spec/state.yaml §m4_kernel.plan_dag_max_nodes`)→环检测(DFS 三色)→深度熔断(`spec/state.yaml §m4_kernel.plan_dag_max_depth`)→孤立节点
L1 确定性 (<1ms, 所有动作): TaintGate + JSON Schema + Tool availability + PolicyGate[Cedar-Gate]
L2 启发式 (<5ms, RiskHigh+): 批量规模(>100)→受保护路径→资源预估
L3 LLM 看门狗 (~200ms, 仅 RiskPrivileged): Tier1 模型语义判断, <10次/小时
```

**ActiveTaintLevel（session 级全局污点）**：当前实现暂设 TaintNone，待 ActiveContext.TaintLevel 正式引入后再从 session 聚合污点读取。per-node 污点已在 `parsePlanOnSuccess` 中按 `pCtx.MaxTaintLevel` 传播，L1 TaintGate 对单节点污点的拦截仍有效。

L1.1 资源冲突检测: 规范 artifactID → 对无依赖边的并行写冲突节点自动注入隐式序列化边 (EdgePrecondition), 审计 `implicit_resource_edge`。

TaintGate (L1 第一道 — `[Taint-Prop]`):
- Layer A 上下文传播: 非系统源 `[Taint-Medium]`+ 数据 → LLM 产出继承上下文最高 `[TaintLevel]` (系统自生成 source∈{compaction, persona_refinement, consolidation, skill_compilation} 排除)
- Layer A.1 工具调用结构化降级: LLM 产出的 DAGNode tool_call 若通过 JSON Schema 校验 (InputSchema + OutputSchema 双向验证)，参数值可经 `[Taint-Sanitizer]` SanitizeBySchema 降一级。字符串字段仅当 schema 定义 format/pattern/enum/const 内容约束时降级（裸 `{"type":"string"}` 不降级），详见 M11 §2.5 SanitizeBySchema。TaintMedium 工具调用 → TaintLow 参数，允许写入 workspace（解除 RAG→代码生成链路阻断，同时确保内容不受限的字符串字段不绕过 Taint 防线）。降级仅在 tool_call schema 校验通过且字段满足内容约束时生效，自由文本 LLM 响应不受此规则影响。每次降级写 `taint_schema_downgrade` 审计事件，标注降级依据
- Layer B 精确子串: `taint_sensitive` 字段 vs active taint set
- 输入反序列化: TaintedJSONNode 递归树 (禁止 map[string]any, 防 Go JSON 剥离污点标记)
- TaintBlocked → HITL → TaintExemptionToken (field_hash+TTL)
- SchemaValidator: Taint 扫描 → InputSchema 校验 → OutputSchema 一致性 → 幂等 ID 合法性

PolicyGate: `[Cedar-Gate]` {principal, action, resource, context} → FORBID 优先

HeuristicChecker (L2, RiskLevel>=RiskHigh): 批量检查(>100) / 受保护路径(`/etc/`,`/sys/`,`~/.ssh/`→拒绝) / 资源预估 vs Tier 阈值

LLMWatchdog (L3, 仅 RiskPrivileged): Budget Pool 模型（M1 §4.2 `<flash-class>`）输出 ALLOW/DENY 判断，频次上限见 `spec/state.yaml §m4_kernel.l3_watchdog_max_per_hour`，超限 → L3+HITL 双审批。L3 为咨询信号——L0/L1/L2 确定性校验未放行的动作不会因 L3 通过而放行。L3 仅可建议拒绝（补充确定性门控），不可建议放行。**DENY 推进 FSM 至 S_VALIDATE_FAIL；ALLOW 或 LLM 不可用时推进至 S_VALIDATE_OK（fail-open）。** 实现见 `pkg/cognition/kernel/agent_execute.go`。

---

## 4. DAG 数据模型与执行

### 4.1 Micro-DAG vs Macro-DAG

| 维度 | Micro-DAG (M4 职责) | Macro-DAG (M8 职责) |
|------|-------------------|-------------------|
| 节点粒度 | 工具调用 (Tool Call) | 子任务 (Sub-task) |
| 边语义 | 工具间数据依赖 / 时序约束 | 任务间产出/验收依赖 |
| 执行边界 | 单 Agent errgroup 并发 | 多 Agent `[Blackboard]` CAS 认领 |
| 生命周期 | Agent Kernel 内部，不发布 | M8 发布到 `[Blackboard]` |
| 所有权 | M4 独占 | M8 编排 / M4 独立执行 |
| Context 隔离 | 共享父 Agent ContextAssembler | **每个子 Agent 持有独立 ContextAssembler 与 context window**，仅通过 Blackboard 结构化 result entry 交换（[Sub-agent-Isolation]） |

> Sub-agent 物理隔离：M8 派发 Macro-DAG 节点时为每个执行 Agent 创建独立 `pkg/cognition/context_assembler.go:ContextAssembler` 实例，禁止共享父 Agent 内存中的 ImmutableCore/MutableSkill/TaintedData zone。子任务结果以结构化 schema（M8 5 原语之 Result）写入 Blackboard，父 Agent 通过订阅 Blackboard 事件消费，避免上下文污染与 token 膨胀(见 00-Global-Dictionary §9-bis [Sub-agent-Isolation])。

### 4.2 数据模型

DAGNode/DAGEdge/EdgePolarity/RetryPolicy/Compensation 类型定义见 `pkg/cognition/kernel/dag_executor.go`（旧版 `pkg/cognition/dag.go` 已标记 Deprecated）。

### 4.3 DAG Executor

DAGExecutor 实现见 `pkg/cognition/kernel/dag_executor.go`（旧版 `pkg/cognition/dag_executor.go` 已标记 Deprecated）。执行流程:
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

实现见 `pkg/cognition/kernel/step_scorer.go`（内核包本地定义，同包调用零 import 开销）。

- **Tier 0 (纯静态启发式)**: 权重 toolSuccess=0.4, schemaCheck=0.3, latency=0.2, tokenEfficiency=0.1。Score 从 1.0 起点按四项扣分，latency/token 惩罚 cap 封顶。
- **Tier 1+ (启发式 + 1.5B 挂载 PRM 融合)**: M1 LocalProvider 加载极小 PRM，对中间步语义打分 (+1,0,-1)，融合权重 0.6。PRM 超时 >100ms 或 OOM → 安全降级纯静态。

**Adaptive Max-Steps 闭环**:
- `StateContext` 持有 `StepsUsed / MaxStepsLimit`；`AgentConfig.MaxSteps` 在首次 `Run()` 时写入 `MaxStepsLimit`（0=无上限，不推荐生产）。
- `Agent.Run()` 每轮 trigger 前计步：`StepsUsed > MaxStepsLimit` → FSM 熔断至 `S_FAILED`，错误码 `MAX_STEPS_EXCEEDED`。
- 每次工具调用后调用 `adjustMaxSteps(current, score)`：score < 0.5 → 收紧 10%（防低质量循环），score ≥ 0.5 → 保持不变（防预算膨胀）。

**Best-of-N 与 Replanning 阻断**:
双路径输出为 Best-of-N 搜索提供置信度排序，低分分支标记为 MEMF 失败候选池，累积低于警戒线立即触发重规划或 Saga 补偿。

### 4.6 ProcessRewardModel — S_PLAN 候选 DAG 选优

实现见 `pkg/cognition/prm/prm.go`（`DefaultPRM`）。与 §4.5 StepScorer 职责不同：StepScorer 对**执行中单步**实时评分；ProcessRewardModel 在 **S_PLAN 阶段**对多个候选整体 DAG 方案打分，选出最优规划后再进入执行。

**触发条件**（`ShouldActivate(complexity float64) bool`）:
- `PRMConfig.Enabled == true` 且任务复杂度 ≥ `ComplexityGate`（默认 0.5）
- 简单任务（问候/天气查询等，复杂度 <0.5）直接跳过，零额外 token 消耗
- 复杂度来源：`StateContext.TaskModel.Complexity`，由 S_PERCEIVE 阶段写入

**多候选并发生成与选优流程**:
```
S_PLAN 阶段检测到 prm != nil && ShouldActivate(complexity):
  1. 并发生成 N 个候选 DAG（默认 MaxCandidates=3，Temperature=0.7 引入多样性）
  2. SelectBest(ctx, goal, complexity, candidates):
       - 并发 goroutine 对每个候选调用 DeepSeek budget-tier LLM（Temperature=0）
       - 评分 JSON schema: {score: number(0-1), reason: string}
       - 全部候选低于 MinThreshold(0.4) → fallback candidates[0]（不丢失规划）
  3. 最优候选 JSON 注入 LLMFillEffect.OnSuccess → 推进 FSM 至 S_VALIDATE
```

**PRMConfig 默认值**:
| 字段 | 默认值 | 说明 |
|------|--------|------|
| Enabled | false | 须显式启用（M8 Orchestrator InjectPRM 注入） |
| ScorerModel | "deepseek-chat" | budget 层，约 ¥2/M token |
| MinThreshold | 0.4 | 低于此分无意义方案，fallback 兜底 |
| MaxCandidates | 3 | 生成候选数（研究数据：3 候选 ROI 最优） |
| ComplexityGate | 0.5 | 低于此值跳过 PRM，零额外开销 |

**Agent 集成接口**: `Agent.InjectPRM(p *prm.DefaultPRM)` 运行时注入，nil 表示禁用多候选路径，单候选走原始 LLMFillEffect 路径。

**DAG 执行结果聚合**：S_EXECUTE 完成后，所有节点的输出通过 `aggregateDAGResults` 统一收集。单节点时直接取其 output（向后兼容）；多节点时序列化为 `{"results":[{id, output}, ...]}` 结构，确保 S_REFLECT 阶段可访问完整 DAG 执行上下文。实现见 `pkg/cognition/kernel/agent_execute.go`。

---

## 5. System 1/2 双轨路由

`[HE-Rule-5]` System 1 物理边界: 零 LLM 调用, 100% 本地 Wasm/Go 技能 + SurrealDB-Core KV 缓存。未命中 → 无条件升级 System 1.5。

| 路径 | `[SurpriseIndex]` | 延迟 | 模型来源 |
|------|-------------------|------|---------|
| System 1 | <0.3 | 亚毫秒 | L0 技能缓存 (零 LLM) |
| System 1.5 | 0.3-0.6 | 毫秒-秒 | M1 Budget Pool |
| System 2 | ≥0.6 | 秒级 | M1 Reasoning Pool |

**与 `[ReasoningEffort]` 正交** (inv_global_05): System 维度由 M4 控制（基于 SurpriseIndex），ReasoningEffort 由 M1 控制（基于 task_type + budget + Provider 能力）。M4 RouteReasoning 不读取 ReasoningEffort 决策；M1 ParallelSampler 不读取 SurpriseIndex 决策。两维度合成矩阵见 [00-Global-Dictionary §9-ter]。

默认 effort 配置: System 1.5 → low; System 2 → medium; 用户 `/set reasoning=high` + Cedar permit → high。

RouteReasoning:
0. si = resolveSurpriseIndex(): 优先读 M3 `polaris_surprise_index`（M9 完整版）→ staleness >60s 回退 `polaris_surprise_index_basic`（M3 基础版）→ 两者均不可用 → 0.5。**`si=0` 为默认零值（未经 M3 计算），不触发 FastPath；正式 FastPath 仅在 `0 < si < 0.3` 时激活。**
1. `0 < si < 0.3` → FastPath：合成 S_PERCEIVE 结果跳过 LLM，S_PLAN 阶段同样旁路 LLM（保留已有 DAGModel 或走空执行路径）。skillCache 命中直接执行 Wasm; 不兼容 fall through
2. 未命中或 si>=0.3 → 调用 `M6.SkillSelector.SelectTopK(intent, K=5)` 选取候选工具/技能描述（**Tool Selection > Tool Design**：避免把全部工具列表塞给 LLM 导致选择崩溃）→ buildMessages → `providerRouter.Route`
3. buildMessages: ImmutableCore + GoalDescription + DAG 上下文 + SkillSelector 选取的 top-K 工具描述。si≥0.6 追加 "extended reasoning"; si<0.3 追加 "use cached skills"

---

## 6. World Model

双层决策体系: L1 World Model 在 LLM 调用前拦截，基于马尔可夫状态转移矩阵（拉普拉斯平滑，公式 `(success+1)/(total+2)`）和 Isotonic Regression 置信度校准，判断当前状态是否可以直接跳过 LLM 推理。校准置信度超阈值 (`spec/state.yaml §m4_kernel.world_model_skip_threshold`) 时跳过 LLM。L2 SurpriseIndex 在执行后进行结果质量评估和路由调整。

**重放确定性契约**: 跳过 LLM 时必须写入 EventLog `event_type='world_model_skip'` 事件（含 StateContext 哈希、转移矩阵版本、置信度、预测输出），重放时从 EventLog 读取该事件直接复用预测输出，禁止重新计算转移矩阵（转移矩阵在重放时刻可能已更新）。这保证 [HE-Rule-5] 状态机控制流的可重放性——World Model 跳过决策本身被视为状态机的一次结构化填空，与 LLM 调用同等待遇。

仿真引擎: 优先使用 SurrealDB-Core KV 中存储的真实快照回放（VCR 模式），未命中时降级为 StatePredictor 的统计估算。反事实推演在 Wasm 沙箱内克隆状态并模拟替代动作，输出 VerificationResult 对比实际结果与模拟结果。

实现见 `pkg/cognition/world_model.go`。

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

M4 不重复实现 TokenBurnRate 检测逻辑，也不独立触发 KillSwitch 阶段变迁。TokenBurnRate 的 CANONICAL SOURCE 是 M3（EMA_5s + EMA_30s），M3 将速率直接推送至 M11 KillSwitch.CheckAndAct（M11 §4.3），这是触发 KillSwitch 阶段变迁的**唯一路径**。M4 通过读取 M11 导出的 `polaris_killswitch_stage` Gauge（Normal=0 / Throttle=1 / Pause=2 / Fullstop=3）获知当前熔断阶段并调整行为:

- **Throttle 阶段**: maxSteps 降至 3，禁止 write_network 操作
- **Pause 阶段**: 优雅完成当前 DAGNode，不启动新任务，等待 M11 恢复或 ESCALATE
- **Fullstop 阶段**: 立即取消所有进行中 LLM 调用，当前任务进入 S_ROLLBACK（仅 Saga 补偿，不重规划）

跨模块交互规则见 `00-Global-Dictionary.md` [XR-01]。

ContextWindowManager（热路径上下文管理，与 M5 SessionCompressor 冷路径协同）:
- maxTokens=90000
- >70% → salience 排序，底 30% 候选交由 M5 SessionCompressor 压缩（M5 §11，LLM 锚定迭代总结）
- >90% → 语义结构感知逐出（以完整 DAGNode/tool_result/Episodic Event 为单位）
- 仍超限 → 触发 M5 Consolidation 全量压缩（M5 §9，跨 Session 语义压缩）

M4 仅持有热路径上下文窗口管理与触发判断；具体压缩算法、锚定策略、cold path 实现委托给 M5（Compaction as First-Class，单一权威源）。

### 7.1 `[ReasoningState]` 跨轮持久化

> 解决 2026 推理模型（o3/R1）的 reasoning_tokens 跨轮不可继承问题。

**问题**: Provider 每次调用都从零开始构建推理状态，跨多轮交互浪费已计费的 reasoning_tokens。

**方案** (HT0 默认 off，Tier 1+ 启用 `FeatureGate.FeatureReasoningStateCarry`):
1. Adapter 在 InferResponse 接收时若 Provider 暴露 `reasoning_state` blob（Claude thinking trace / OpenAI encrypted_reasoning），M4 将其写入 M5 `episodic_events.reasoning_state` 列（msgpack + AES-256-GCM 加密，[CredentialVault] persistent_key 派生）
2. 下次 LLM 调用前，若 task_id 相同且 ≤30min 内，M4 从 M5 读取最近 reasoning_state 注入 InferRequest 作为推理前缀
3. 跨任务/跨 session/超时 → 不继承（防止上下文污染 + 隐私边界）
4. 加密落盘（reasoning_state 可能含敏感推理痕迹，与 PII 同级保护）
5. SessionPIIVault.SecureZero 同步清理 reasoning_state

**约束**:
- ReplayMode 下 reasoning_state 也从 EventLog 重放，保证确定性
- 仅 reasoning Provider（o-系列 / DeepSeek R / Claude thinking）支持，其他 Provider ignored

---

## 8. Crash Recovery

满足 [HE-Rule-6]（State-in-DB）——崩溃恢复从 M2 EventLog 回放，不依赖显式 FSM checkpoint。SurrealDB-Core KV 的 goroutine checkpoint（§3）仅用于空闲时释放 goroutine 栈以节省内存，非崩溃恢复路径。

**回放机制**: M4、M5、M11 启动时统一检查 [ReplayMode] 标志（进程级 atomic.Bool）。回放期间禁止所有外部副作用（EmitEvent/ToolCall/Outbox）——纯函数式重建内存状态。追平事件流后退出回放模式，从崩溃点继续执行。

**网络抖动恢复**:
触发: 长任务 (>5min) 每 5min 或每 10K tokens 推理输出 → SnapshotContext
写入: SurrealDB-Core KV `session_snapshots` namespace，key=`{session_id}:{seq}`，TTL=24h，per session 上限 5
events 表: 轻量 `source='snapshot_checkpoint'` 记录（含 SurrealDB-Core key 引用），供时序定位
PII: 快照不含明文——ToolResult 经 M7 §4.3 Step 5 PostExecution Redact 后写 EventLog；FSM Snapshot 保留原始值供同 session 崩溃恢复
恢复: 优先加载最近快照 → 差量回放后续事件；快照损坏 → 回退 EventLog 全量回放（ToolResult 红化版本，需 SessionPIIVault 仍存活解析 token）
与 M5 SessionResume 共享同一 barrier 协同重建。

**双重幂等防线**: 第一层 isReplaying 标志 物理切断副作用；第二层 UNIQUE(session_id, seq) 约束 + idempotency_key 保证重复事件的幂等消费。

**Replay Key 算法（录像 key）**: 所有写入 EventLog 的事件 ID 必须满足重放确定性——同 session+seq → 同 ID，不依赖 wall clock。算法定义:

```
Event ID 格式: {session_id}:{seq}:{event_type}
- session_id: 当前 Agent 会话唯一标识
- seq:        StateMachine.eventSeq 单调递增计数器（自 StateMachine 创建起，每产生一个事件 +1）
- event_type: 事件类型标识（perceive / plan / exec / action_pending / action_done）

生成函数: StateMachine.nextEventID(sessionID, eventType)
代码位置: pkg/cognition/kernel/state_machine.go
重新调 LLM 检测: 回放时校验 EventLog 中 event_type+ID 与 FSM PrompFn 占位符的对应关系，
              任何缺失匹配 → 触发 g_inv_08 防护（禁止重新调 LLM，进入 REPLAY_MISMATCH 审计）。
```

在非重放路径上（`uuid.New().String()` 生成 2PC 中间事件），同一事件的重放时间戳不同但通过 idempotency_key 防重入——回放时 `isReplaying=true` 物理切断所有副作用，确保这些 UUID 事件不会被重新投递。

**Snapshot 策略**: 步频与保留数见 `spec/state.yaml §m4_kernel.snapshot_interval_steps` / `snapshot_retention_count`。Snapshot 损坏时回退到完整 EventLog 回放。

**S_REPLAN 降级**: M1 CircuitBreaker 熔断时，执行零 LLM 的确定性图剪枝（纯 Go 图遍历）：移除失败节点及其所有直接后继节点，注入 degraded_replan 标记。此步骤禁止任何 LLM 调用——剪枝逻辑为纯函数，幂等且可重放。

**`ErrAllProvidersExhausted` 专项处理（全 Provider 熔断）**:
  1. 确定性图剪枝后检查剩余 DAG 节点:
     (a) 有 System 1 可执行节点（SurpriseIndex <0.3，零 LLM，纯本地 Wasm/Go 技能）→ 继续执行，**不消耗 ReplanCount**；LLM 依赖节点等 Provider 恢复
     (b) 全部需 LLM → **不消耗 ReplanCount**，转 `Suspended(suspend_reason=provider_exhausted, provider_suspended_count++)`；Blackboard 写标记；调 `SessionPIIVault.SuspendSnapshot(ctx, taskID)` 持久化 PII
  2. `provider_suspended_count > 5` → 终止自动唤醒，触发 `[ESCALATE]` + HITL
  3. 剪枝后剩余 DAG 为空 → `[ESCALATE]` 人工审批

  **Provider 恢复唤醒**: M1 CircuitBreaker Open→Closed (§7.3) → M2 Outbox 投递 `target_engine:"m4_provider_recovery"` 事件。Handler 注册于 M2 全局 Outbox Worker (`pkg/substrate/outbox_worker.go`)，实现位于 `pkg/cognition/kernel/recovery.go`——不在 M4 内独立 Worker（违反 M2 §2.3 单写者）。执行序列:
    1. 扫描 M8 Blackboard 全部 `suspend_reason=provider_exhausted` 任务
    2. 逐一 `M11.SessionPIIVault.RestoreFromSnapshot(ctx, taskID)` 解密恢复 PII token
    3. `M8.Blackboard.ResumeFromSuspended(taskID)` 重置 Suspended→Pending
    4. 重新调度（M8 ListenLoop 扫描认领）

**FSM 终态 PII 清零**: M4 转 S_FAILED / S_COMPLETE 时，先于 WorkspaceManager GC 调 `SessionPIIVault.SecureZero(ctx, taskID)`，pii_vault_blob 先于 workspace 删除（GDPR 主动擦除）。无可执行节点 → `[ESCALATE]`。

**Saga 补偿**: 确定性函数 + 预定义 HTTP 模板，禁止 LLM 参与。补偿前 M11 PolicyGate.Review 预检——FORBID → `[ESCALATE]` + `compensation_blocked_by_policy_revocation` 审计。非权限型失败重试 3 次（exponential backoff）。

完整时序见 `DIAGRAMS.md#eventlog`。

---

## 12. 降级与失败模式（5 问全覆盖）

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


## 13. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces.go` + `types.go`。本表仅列依赖方向 + 一句话语义 + 锚点。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M4→M1 | Provider.Infer / StreamInfer | LLM 推理；SurpriseIndex consumer。M1 §2, §4 |
| M4→M2 | EventLog Append / GetEvents | 崩溃恢复回放真相源。M2 §2.1 |
| M4→M2 | Outbox `m4_provider_recovery` handler | `pkg/substrate/outbox_worker.go` 注册；实现 `pkg/cognition/kernel/recovery.go`。M2 §2.5, M4 §8 |
| M4→M3 | OTel spans + SurpriseIndex 消费 | 双层回退 完整版→基础版→0.5。`[HE-Rule-1]` M3 §4 |
| M4→M5 | ContextAssembler / HybridRetriever | 记忆检索 + 上下文组装。M5 §2, §7 |
| M4→M6 | SkillLookup / SkillRegister | System 1 技能缓存 + Persona 兼容性。M6 §3, §4.3 |
| M4→M7 | ToolRegistry.ExecuteTool | S_EXECUTE 节点调用 `[Wasm-Sandbox]`。M7 §3 |
| M4→M11 | TaintGate / PolicyGate / KillSwitch | 查阅，仅响应不主动触发。M11 §2, §4 |
| M4→M11 | SessionPIIVault | Suspend 时落 pii_vault_blob；Restore/SecureZero 跟随 FSM 终态。M11 §5.1 |
| M8→M4 | Blackboard.CAS Claim / LeaseHeartbeat | 多 Agent 调度入口。M8 §1 |
| Schema | AgentState 枚举、DDL 001_events / 003_episodic_memory / 003_tasks（含 pii_vault_blob / suspend_reason / provider_suspended_count）| `internal/protocol/types.go`, `internal/protocol/schema/` |
| 全局字典 | HE-Rule-5 状态机控制流、XR-01 | 00-Global-Dictionary §1-bis, §1-ter |
| 时序图 | EventLog 回放、KillSwitch 响应链 | DIAGRAMS.md#eventlog, #killswitch |

`[Module-Topology]` `[Code-Package-Mapping]`

---

## 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m4_kernel`。
