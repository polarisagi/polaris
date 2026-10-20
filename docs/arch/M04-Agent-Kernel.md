# 模块 4: Agent Kernel

> Go 状态机持有控制流，LLM 仅概率性填空。`[HE-Rule-5]` `[Tier-0-Limit]`
> 代码位置：`internal/agent/`（FSM 在 `internal/agent/fsm/`；DAG 执行器与 S_VALIDATE 四层校验物理实现在 `internal/execute/dag/`，ADR-0046；FSM 经 `internal/agent/provider.go` 的 `DAGRunner`/`DAGValidator` 消费端接口驱动，唯一决定何时进入 S_EXECUTE/S_VALIDATE）。
<!-- §跳读: 0-bis:6 职责 / 0-ter:19 不变量速查 / 1:37 状态机 / 1.1:100 输出通道+S_RESPOND / 2:172 Suspend-on-Idle / 3:190 S_VALIDATE / 4:229 DAG（Directed Acyclic Graph，有向无环图） / 5:299 System1/2 / 6:323 WorldModel / 7:331 推理预算 / 8:378 CrashRecovery / 8-bis:416 Handoff唤醒事件化+无损续跑 / 12:426 已知Bug修复记录 / 13:432 (SOFT)降级 / 14:449 481 跨模块契约 / 15:473 505 默认参数 -->
## 0-bis. 职责边界

| M4 **是** | M4 **不是** |
|-----------|-------------|
| 单 Agent 任务的确定性状态机执行器 | LLM 客户端 / Provider 选择（M1） |
| System 1/1.5/2 路由决策（基于 SurpriseIndex） | 跨 Agent 协调（M8） |
| DAG 规划与并发执行控制 | 记忆持久化与检索（M5） |
| 决定何时调 LLM / 何时调 Tool | 工具沙箱执行（M7）、技能发现与匹配（M6） |
| 崩溃恢复（EventLog 回放 + 轨迹回放） | |
| Prompt 组装（Slot 分离 + Taint 门控） | |

---

## 0-ter. 不变量速查表

| 编号 | 不变量 | 验证方式 |
|------|--------|---------|
| inv_M4_01 | LLM 仅做结构化填空——Go 状态机持有控制流，禁止 `while True: call LLM` | spec/state.yaml FSM（Finite State Machine，有限状态机） 校验 |
| inv_M4_02 | 重放时不重新调 LLM——用 EventLog 录像值（请求全文 + 响应全文） | M4 §8 ReplayMode 物理切断 |
| inv_M4_03 | PromptFn 为纯函数——同 StateContext → 同 prompt 字节，禁止 wall_clock/random | CI（Continuous Integration，持续集成） `prompt_determinism` 测试 |
| inv_M4_04 | 见下方 [inv_M4_04 补充](#inv_m4_04-补充) | M4 RouteReasoning 代码审计 |
| inv_M4_05 | Suspend-on-Idle——空闲 Agent 不轮询，等待 intent channel 唤醒，空载 CPU<1% | M3 `polaris_runtime_goroutines` Gauge |
| inv_M4_06 | 不可逆操作（write_network/privileged）禁止自动回滚——必须显式 HITL（Human-in-the-loop，人机协同） | M7 §5.3 DryRunMode + HITL |

> 进入此模块前必读 `docs/specs/09-LLM-Agent-Production.md`（HE-Rule-5 实例: A-02/A-09/A-11 + P-1/P-6/P-7）

#### inv_M4_04 补充
System 1 路径零 LLM 调用——`0 < SurpriseIndex < 0.3` 时触发 FastPath（0 表示"未计算"，不触发）；FastPath 在 S_PERCEIVE 合成结果、S_PLAN 旁路 LLM（保留已有 DAGModel 或走空执行路径），不操作 DAGModel/ExecuteResult。

---

## 1. 状态机

> [!NOTE] 状态机架构演进与冻结状态（ADR-0115）
> 经典阶段式状态机（staged 模式：S_PERCEIVE → S_PLAN → S_VALIDATE → S_EXECUTE → S_REFLECT → S_RESPOND）已**冻结**，保留作为程序兜底机制与对比基线。
> 默认运行模式为自主循环架构（agentic 模式：见 §1.2），由模型原生驱动工具调用，程序保证安全闸门与失控熔断。

状态枚举权威定义：`pkg/types/enums_agent.go`（AgentState 共 **18 态**：Idle/Perceive/Plan/Validate/Execute/Reflect/Replan/Rollback/Complete/Failed/Interrupt/Suspended/AwaitAgent/Respond/Loop/LoopExec/LoopAwait/LoopRecall，ADR-0098 决策二 / ADR-0115 / R4-02）。转移注册：`internal/agent/fsm/transitions.go` + `internal/agent/fsm/transitions_respond.go` + `internal/agent/fsm/loop_transitions.go`。`[HE-Rule-5]` LLM 结构化填空三态输出：TaskModel(S_PERCEIVE) / DAGModel(S_PLAN) / ReflectionModel(S_REFLECT)；S_RESPOND 是唯一面向用户的自由文本态（§1.1）。

分类：6 经典主执行态（Perceive/Plan/Validate/Execute/Reflect/Respond）、2 经典恢复态（Replan/Rollback）、1 中断态（Interrupt）、1 挂起态（Suspended：Suspend-on-Idle 与 provider_exhausted）、1 经典等待态（AwaitAgent：Handoff 子任务终态前阻塞，`TriggerAwaitAgent` 进 / `TriggerAgentHandoffDone` 出）、2 终态（Complete/Failed）、Idle、4 自主循环态（Loop/LoopExec/LoopAwait/LoopRecall）。

`ReflectionModel` = `{GoalAchieved bool, Errors []string, Learnings []string}`。`onReflectSuccess` 解析后 `Learnings` 非空则逐条写入 episodic（`sCtx.Mem.Episodic().Append`，EventType="learning"）；写入失败仅 WARN，不阻断流转。

```
S_PERCEIVE ──(LLM_fill 理解任务)──→ S_PLAN ──(LLM_fill 生成 DAG)──→ S_VALIDATE ──┬──OK──→ S_EXECUTE ──┬──OK──→ S_REFLECT ──→ S_RESPOND ──→ S_COMPLETE
    │                                 │                                          │              │                         ↑
    │ NeedsTools=false                │ DAG 为空                                 └──Fail─→ S_REPLAN ─┘  └──Fail─→ S_ROLLBACK ──→ S_REPLAN
    └─────────────────────────────────┴──────────────→ S_RESPOND（直答）──────────────────────────────────────────────┘
                                                    ReplanCount ≥ MaxReplanAttempts: 转 S_RESPOND 说明失败（TurnDegraded，结果按失败计；ADR-0098 决策九）
                                                    S_REFLECT(目标未达成) ──reflect_continue──→ S_REPLAN（决策八，共用 ReplanGuard）

  任意态 ──(UserInterrupt / KillSwitch)──→ S_INTERRUPT ──┬──Resume──→ 原状态
                                                          ├──Redirect─→ S_PLAN (用户修正意图)
                                                          └──Abort────→ S_FAILED
```

**`[UserInterrupt]` 协议**（inv_global_08, < 200ms 传播）:
- **触发**: M13 `POST /v1/agent/{taskID}/interrupt` (M13 §1.2.5) → EventLog Subscribe 与 `agent.ContextCancel()` 内存通知推送至 Agent（中断信号不落盘，`007_tasks.sql` 无 `interrupt_pending` 列）
- **进入 S_INTERRUPT**: `agent.ContextCancel()` 立即取消 → 所有 LLM call / tool call / [BestOfN] ParallelSampler 子 goroutine 同步终止
- **Resume**: 用户提供"继续"指令 → 恢复原状态 + 注入用户指令到 ZoneImmutable（`source='user_interrupt'`, [TaintLevel]=TaintUserReviewed）
- **Redirect**: 用户修正意图 → S_PLAN 重新规划 + 保留原 EventLog（不消耗 ReplanCount）
- **Abort**: S_FAILED + Saga 逆序补偿 + workspace GC
- **持久化**: `tasks.suspend_reason='user_interrupt'` + 进入 [Suspend-on-Idle] 等待用户响应
- **SLO**: 触发 → context cancel < 200ms（与 [KillSwitch] FULLSTOP 同级）；M3 `polaris_user_interrupt_latency_ms` Histogram

转移表（权威：`transitions.go` / `transitions_respond.go`；全局中断处理器在 `Dispatch()` 中早于本表检查）:

| From | Trigger | To |
|------|---------|-----|
| S_IDLE | TriggerIntentReceived | S_PERCEIVE |
| S_IDLE | TriggerSuspend | S_SUSPENDED |
| S_SUSPENDED | TriggerResume | S_IDLE |
| S_PERCEIVE | TriggerPerceiveDone | S_PLAN |
| S_PERCEIVE | TriggerRespondReady | S_RESPOND |
| S_PERCEIVE | TriggerValidateOk | S_EXECUTE（System 1 旁路后四层校验通过） |
| S_PERCEIVE | TriggerValidateFail | S_REPLAN（System 1 旁路后校验失败） |
| S_PLAN | TriggerPlanDone | S_VALIDATE |
| S_PLAN | TriggerRespondReady | S_RESPOND |
| S_PLAN / S_RESPOND | TriggerFillRetry | 自环（空输出重试，ADR-0098 决策七） |
| S_VALIDATE | TriggerValidateOk | S_EXECUTE |
| S_VALIDATE | TriggerValidateFail | S_REPLAN |
| S_EXECUTE | TriggerExecuteDone | S_REFLECT |
| S_EXECUTE | TriggerExecuteFail | S_ROLLBACK |
| S_EXECUTE | TriggerAwaitAgent | S_AWAIT_AGENT |
| S_AWAIT_AGENT | TriggerAgentHandoffDone | S_EXECUTE |
| S_REFLECT | TriggerReflectDone | S_RESPOND |
| S_REFLECT | TriggerReflectContinue | S_REPLAN |
| S_ROLLBACK | TriggerRollbackDone | S_REPLAN |
| S_ROLLBACK | TriggerRollbackPartial | S_FAILED（补偿部分失败 → `[ESCALATE]`） |
| S_REPLAN | TriggerReplanDone | S_PLAN |
| S_REPLAN / S_PERCEIVE / S_PLAN / S_REFLECT | TriggerReplanExhausted | S_FAILED |
| S_RESPOND | TriggerRespondDone | S_COMPLETE |
| S_RESPOND | TriggerReplanExhausted | S_FAILED |

状态超时: S_PLAN 300s、S_EXECUTE 600s（计算性）；S_PERCEIVE/S_VALIDATE/S_REFLECT/S_RESPOND/S_REPLAN 为 `upstream_budget - elapsed`，安全地板 30s；S_IDLE/S_COMPLETE/S_FAILED/S_ROLLBACK 无超时。

**ReplanGuard**（S_REPLAN 入口；阈值 `spec/state.yaml §m4_kernel.max_replan_attempts`）覆盖 5 条路径：S_VALIDATE 失败 / S_ROLLBACK 完成 / M1 FatalStreamAbort / M1 JSON Repair 失败 / S_PLAN 拓扑失败。超限处置（ADR-0098 决策九，2026-09-25）：经 S_VALIDATE/S_ROLLBACK/S_REFLECT 进入 S_REPLAN 时预算已满 → 转 S_RESPOND 说明失败（`TurnDegraded`，任务结果按失败计）；`TriggerReplanExhausted → S_FAILED` 用于内核错误、KillSwitch、预算硬上限与感知/规划/回复阶段自身失败，并 `[ESCALATE]` 移交 M13 HITL。S_FAILED 为终态——不进入 S_ROLLBACK、不触发补偿。FSM 唯一生产实现：`internal/agent/fsm/state_machine.go`（`StateMachine`）。

### 1.1 输出通道与 S_RESPOND（ADR-0098）

- **受众**：`protocol.LLMFillEffect.Audience` 零值 `AudienceInternal`（fail-closed）；仅 S_RESPOND 的 Effect 为 `AudienceUser`。`doStreamInfer` 只对 User 受众发布 `AgentStreamEventToken`，内部阶段 token 仅累积供 OnSuccess 解析（`spec/state.yaml par_inv_06`）。思考链各阶段照常发布，不进回复正文。
- **阶段进度**：每个 LLMFillEffect 开始发布 `AgentStreamEventPhase`（Content=`perceive|plan|reflect|respond`），DAG 执行开始发布 `execute`；session 映射为 `status{type:"phase"}`。
- **路由**：S_PERCEIVE 产出 `TaskModel.NeedsTools`（`*bool`）。`false` → `S_PERCEIVE_DIRECT` → S_RESPOND；`true`/缺失/解析失败 → S_PLAN（保守）。S_PLAN 解析成功但 DAG 为空 → `S_PLAN_EMPTY` → S_RESPOND。
- **零 LLM 寒暄快路**（ADR-0105，原 ADR-0102 决策一）：`fsm.ClassifyIntentWeight` 判 `IntentPhatic`（问候/致谢/告别，≤16 rune 整句匹配）时，S_IDLE→S_PERCEIVE 的 Effect 为 `DeterministicEffect`，直达 `S_PERCEIVE_DIRECT`（route=`phatic_bypass`），不调 Perceive、不召回记忆；`IntentAck`（好的/ok/同意）仍走 Perceive（可能授权上一轮提议动作），跳过长期记忆召回。
- **直答合并**（原 ADR-0102 决策四 4b′）：`NeedsTools=false` 时 Perceive 同次输出 `Reply`；经 `publishableReply` 检查存入 `PreparedReply`，仅 Perceive→Respond 入边以确定性 Effect 发布（route=`direct_merged`），回合起点清空；缺失/被拦截 → 照常 Respond LLM。
- **简单任务跳过反思**（原 ADR-0102 决策四 4a）：首轮、`ExecAllSucceeded` 且 0 < Complexity < `m4_kernel.reflect.skip_complexity` 时 S_REFLECT 为 `DeterministicEffect`（route=`reflect_skipped`）→ S_RESPOND。
- **工具定义去重**（原 ADR-0102 决策五）：S_PLAN 文本目录只列工具名，完整定义仅经原生 function-calling 下发；无原生 tools 的本地适配器自行渲染文本。
- **规划池阶梯**（原 ADR-0102 决策六 + `model_pool.plan_initial/plan_replan`、`thinking.plan_initial`）：Perceive 判 `Complexity`，程序经 `SelectPlanTier` 定池与思考档；仅能力类失败（L0 结构错误、计划不可解析、重复工具报错、规划模型 `escalate=true`）逐级升级，安全拒绝/瞬时故障/观察—再规划不升级；规划输出有内容但不可用时升级重试一次（每回合一次）。
- **空输出重试**（ADR-0098 决策七）：S_PLAN/S_RESPOND 推理成功但既无正文也无工具调用 → 按 Effect `MaxRetry`（=1）经 `TriggerFillRetry` 自环；推理错误不重试（Router 已全量 failover）；仍为空 → S_FAILED，原因以错误事件进入事件流。
- **观察—再规划循环**（决策八）：`GoalAchieved` 显式 false 且 `replanCount+1 < MaxReplan` → `reflect_continue` → S_REPLAN；每轮执行结果累积为 `StateContext.Observations`（最近 4 条、每条 ≤4KB），超限首尾保留截断，全文经 `ToolRefOffloader` 卸载并给出 `read_tool_ref` 取回提示（→ ADR-0020 溢出/spill 决策）。各阶段思考档位由 `m4_kernel.thinking.*` 配置，记忆蒸馏在回合终态触发一次。Provider 以 `protocol.ErrContextOverflow` 拒绝时，对固定前缀外的消息确定性修剪一次后重试（`internal/agent/agent_overflow_recovery.go`）。
- **重规划闭环**（决策六）：S_VALIDATE 拒绝 / S_EXECUTE 失败原因写入 `StateContext.ReplanFeedback`（最近 3 条，每条 ≤400 字节），S_PLAN 与 S_RESPOND prompt 均携带（TaintMedium 数据区）。无允许方案 → Plan 返回空计划 → S_RESPOND 说明限制。
- **步数上限**：`m4_kernel.max_steps`（回合内 FSM 触发次数，覆盖完整工具回合 + 观察循环 + 校验失败重规划 + 空输出重试 + 耗尽转回复）；截断经 `abortTurn` 收尾。
- **S_REFLECT 失败**：反思尽力而为，LLM 失败仍转 S_RESPOND（不带反思结论），不丢弃已执行回合的回复。
- **阶段契约 SSoT**：`configs/prompts/kernel/perceive.md`、`plan.md`、`reflect.md`、`respond.md`，记忆路径与降级路径同源加载；Perceive/Reflect 请求 `json_object` 约束解码。

### 1.2 自主循环内核（Agentic Loop，ADR-0115）

自主循环内核采用「模型驱动、程序兜底」架构（ADR-0115），模型直接面对会话上下文生成文本或发起工具调用：

```text
S_IDLE ──(TriggerIntentReceived)──→ S_LOOP_RECALL ──(TriggerLoopRecallDone)──→ S_LOOP ──(TriggerLoopStepDone)──→ S_LOOP_EXEC ──(TriggerLoopExecDone)──→ S_LOOP ──(TriggerLoopDone)──→ S_COMPLETE
                                                                                 │                                  │                                     ↑
                                                                                 │                                  ├──(spawn_subagent wait=all)          │
                                                                                 │                                  ↓                                     │
                                                                                 │                             S_LOOP_AWAIT ──(TriggerLoopHandoffResume)──┘
                                                                                 │                                                                        ↑
                                                                                 └──(无工具调用 / 最终回答)────────────────────────────────────────────────┘
```

1. **回合结构与跨轮隔离（AgenticTurn，R2-01）**：引入结构化 `AgenticTurn`，首轮自动将用户意图（`RawIntentTS`）作为 Round 1 user 消息注入模型上下文。回合生命周期由 `ResetAgenticTurn()` 统一管理，每回合清零瞬态轮次、消息列表、待执行调用、预算计数、警告标记与插话队列，并派发新的 `TurnID`，彻底防止跨回合状态残留与答非所问。
2. **工具并发安全规则与分组调度（R2-06）**：单轮中多个工具调用根据 `ConcurrentSafe(toolName)` 进行语义分组——只读与纯计算工具（如 `read_file`, `web_search`）作为只读组由 `concurrent.SafeGo` 并发执行；并发不安全工具（写状态、bash 持久会话等）严格按批次单调串行执行，阻断数据竞争与状态混乱。
3. **子智能体挂起等待与单批次多委派（S_LOOP_AWAIT，R2-03 / R3-05）**：当模型在自主循环中调用 `spawn_subagent` 且指定 `wait=all` 时，智能体从 `S_LOOP_EXEC` 自动挂起并转移至 `S_LOOP_AWAIT`（状态码 16）。内核支持单批次并发发起多个等待型委派（`LoopPendingDelegations` 映射表），记录每个任务的 `task_id` 与对应 `tool_call_id`。后台 Blackboard Watcher 异步监听任务终态，并在所有等待型委派全部完成后触发 `TriggerLoopHandoffResume` 唤醒父智能体切回 `S_LOOP`，将所有子任务输出原语精确回填为对应 `tool_result`，并作为当前轮次增量完整持久化。
4. **轮次增量持久化（agent_loop_rounds，R2-05）**：摒弃静态单条快照方案，按 `(session_id, turn_id, round)` 表粒度增量记录每轮推理与执行的 delta（含脱敏后消息与污点定级），配合 `ResumeAgenticTurn` 提供进程崩溃后的精确无损续跑能力。
5. **前缀缓存友好与末尾易变块布局（KV Cache & Tail Volatile Block，R3-01）**：严格保持不可变稳定核（`ImmutableCoreFields`），System Prompt、工具元数据与覆盖层提示词（`prompt_addenda`）在会话生命周期内前缀完全一致。为了解决运行时易变数据（时间戳、系统负荷、用量计量行 `[usage]`）位于消息前部导致 Anthropic/OpenAI 前缀缓存穿透的问题，内核将所有跨轮易变状态统一下沉至消息流末尾的单条 `Role: "user"` 临时易变块中下发（前置带 `[system_context]` 标头，且在下轮追加真实消息时自动剔除上一轮易变块），保证历史消息序列（System + User/Assistant/Tool 轮次消息）的前缀字节单调追加，KV Cache 命中率稳固在 50%~65%，端到端推理成本大幅降低。
6. **安全闸门不旁路**：循环中每个工具调用严格经过 `S_VALIDATE`，执行参数校验、路径守卫（`path_guard`）、Cedar 策略评估以及出站污点检查，高风险操作需持有 HITL 豁免令牌；所有工具执行结果与版本回滚均写入审计事件轨迹。
7. **双重失控保护与易变收尾提示（R2-17）**：
   - 软保护：达到 `LoopWarningRounds` 或首触 USD/Tokens/Wall 预算上限时，向易变区（与 T7 计量行同位）注入 `Role: "user"` 的收尾告警提示词，严禁使用 system 角色或追加至历史末尾，避免污染消息流与破坏 Anthropic 前缀缓存；
   - 硬保护：达到 `RunawayGuardDefaultRounds`（standard 200 轮，hardened 50 轮）或次轮仍未收尾时强行熔断并转移至 `S_FAILED`。
8. **网关插话与污点定级（R4-05）**：用户在多轮循环执行期间可随时发送新消息或转向指令（Steer）。内核将插话在入队时封装为 `taint.TaintedString`（`OriginTaintLevel: TaintHigh`），在主循环单线程消费注入时统一执行 Spotlighting 并提升全局污点等级，确保下一轮任何写类或网络类工具调用均受到严格的 TaintHigh 权限拦截与审计。
9. **代码执行（code_act）与持久会话（R3-02, R4-01, R4-03）**：自主循环模式下，工具列表中下发 `code_act` 工具 schema（支持 Python/Bash 脚本与专用 `polaris.call` 工具桥，仅 print 输出返回）。执行结果严格执行 `taint.SanitizeToSafe` 污点清洗；schema 明确声明 `session_id`（string）参数，当调用方传入非空 `session_id` 时自动开启持久会话，隔离键限定于当前 Agent 会话空间（`agentSessionID:session_id`），杜绝跨会话串用与变量污染。
10. **控制通道与事件流解耦（R3-04）**：子智能体（如 Headless Worker、Subagent Runner）的控制通道与事件流彻底解耦。控制指令（Stop / Cancel / Interrupt）经由独立的专用控制 channel 发送，严格通过正确的全局会话标识路由；业务事件流（StreamCallback / EventCallback）仅用于向下游或父 Agent 投递感知、执行与输出事件。彻底杜绝将控制信令注入事件流或会话名混淆引起的死锁与回环。
11. **回合级完成模式与防浪费兜底（finish 工具，R3-07, R4-04）**：完成模式（`LoopCompletion`）从会话级彻底下沉为回合级字段（`AgenticTurn.LoopCompletion`），回合默认值为 `auto`（模型直接给出文本即自然完成），解除按会话复用 Agent 实例时对人类对话渠道的错误黏附。仅由调用方（`default_worker`、`cron`、`workflow`）显式声明时才开启 `explicit` 模式并下发 `finish{summary: string}` 工具；若模型在 explicit 模式下连续 2 轮仅输出纯文本而未调用 `finish`，内核自动触发防浪费兜底（`implicit_finish`），提取最后一段文本作为任务总结，打点 `polaris.agent.loop_termination_total{reason="implicit_finish"}` 并安全收尾，避免消耗至 200 轮硬熔断上限导致 token 巨额浪费。
12. **统一轮次追加入口与后台异步崩溃恢复（R3-06, R4-06）**：内核将所有循环消息追加入口统一下沉至 `fsm.StateContext.AppendLoopMessages`，加锁统一标记当前轮次 `Round`，杜绝遗漏轮次标记导致的增量持久化丢失；门控测试全域递归扫描禁止直接改写 `LoopMessages`。启动路径上的崩溃循环恢复（`recoverAgenticLoopTurns`）移至后台受限并发（≤ 2）运行，启动函数 1 秒内非阻塞返回，避免海量悬停任务阻塞系统启动主路径。
13. **回合自动记忆召回（S_LOOP_RECALL，R4-02）**：自主循环回合起点引入确定性 Effect 步骤 `S_IDLE → S_LOOP_RECALL（状态 17）→ S_LOOP`。在 FSM 锁外调用统一记忆召回管线（`TurnRecallText`），召回情景记忆、反思、L2 语义与 RAG 知识库（`RecallWant.Goal` 传入本回合意图；缺 Goal 则只查情景，R5-03），结果以 `## Recalled Context` 注入本回合首条 user 消息，并合并污点定级（`TaintLevel`）。若召回失败，自动降级为无召回并记录指标 `polaris.agent.loop_recall_failure_total`，不阻断回合。寒暄快路（`tryPhaticBypass`）跳过召回。 召回非空时与 staged 的 `writePlanRecall` 同口径：`GlobalTaintLevel` 至少提升到 `TaintMedium`，首条消息污点取 `PropagateTaint(用户意图, Medium, Global, 召回项)`（R5-03）。
14. **交互回合流式输出与工具进度（R5-02）**：`loopEffect` 在「交互回合」（`LoopCompletion != explicit` 且非 `AcquireHeadless` 获取，`StateContext.LoopStreamsToUser`）设 `Audience=User`——每轮文本增量以 Token 事件流式推送，工具调用事件由 `acceptStreamToolCall` 推送，`executeLoopToolCall` 完成后补发 `ToolResult` 事件（内容截断 500 字符，污点随结果）。已流式推送的回合以 `LoopReplyStreamed` 标记，终态 `publishPreparedReply` 不再重发；`ConversationHistory` 中的助手内容取各轮流式文本按序拼接（`LoopStreamedText`），无流式文本时才回落 `PreparedReply`/`LoopSummary`/最后一段助手文本。委派子智能体（explicit）、cron、workflow 等无人阅读的回合保持回合末一次性发布（`AcquireHeadless` 靠它拼 Output）。par_inv_06 已据此追记复核（`spec/state.yaml`）。
15. **子智能体嵌套输出不入父回复正文（R5-01）**：`IsNested` 的 Token 事件在网关 `handleFSMEvent` 转为 `KindStatus{type:"subagent_output", agent_id, parent_id, role, message}` 进度事件，不进 `*reply`、不推 `KindDelta`、不持久化（子智能体结果本就经 tool_result 回到父上下文）；系统提示词泄露扫描仍作用于该内容。前端在子智能体树节点下展示。
16. **空输出恢复（R5-06 / 2026-10 修订）**：某轮既无正文也无工具调用（典型：只产出 reasoning）时，`onLoopEmptyOutput` 经 `S_LOOP_STEER` 最多再调用 **2 次**（`LoopEmptyRetries`，连续计数，出现正文/工具调用即清零），**绝不原样重发同一请求**：① 第 1 次重试：请求末尾追加瞬态 assistant 占位 + user 提示“上一轮只完成了思考，没有发出工具调用或回答……”，并把上一轮 `reasoning_content` 回传（DeepSeek 思考模式要求）；② 第 2 次：本回合已有工具结果 → `tool_choice=none` 的无工具收尾调用（`loopEmptyRecoveryOpts`）；尚无工具结果 → 关闭思考（`ThinkingDisabled`）再试一次。仍为空且从未产出正文 → `S_LOOP_FAILED`，错误文案对用户友好（不再是 "LLM returned empty output after retry"）。瞬态消息不进 `LoopMessages`，不污染历史与前缀缓存。诊断：`llm fill unusable` 日志带 `content_bytes/fill_bytes/reasoning_bytes/tool_calls/finish_reason/output_tokens`，适配器 Debug 日志 `llm stream: summary`，以及 `POLARIS_LLM_RAW_DUMP`（见 M03 §8-bis）。
18. **agentic L0 不含阶段契约（2026-10）**：自主循环的稳定核只写 `ImmutableCore.StableMessage()`，不再注入 `# PHASE CONTRACTS` / `# ACTIVE PHASE`（Perceive/TaskModel JSON 等分阶段契约对“模型直接驱动”的循环是噪声，且诱导模型输出结构化 JSON 或只思考不行动）；循环指令走 `kernel/loop.md`（`BuildAgenticPrompt`）。系统提示词 token（4 字节/token 估算）2335 → 537；前缀仍字节恒定（`agentic_prompt_test.go` 断言）。四阶段降级路径的契约并入 L0（决策九）不受影响。
17. **主循环模型池（R5-07）**：`m4_kernel.model_pool.loop`（默认 `default`）决定主循环每轮的模型池，不再复用参与升级阶梯的 `model_pool.plan_initial`；模型自选（`resolveModelRoute`）与用户手选锁定的优先级不变。

**回合上下文（ADR-0105）**

- **五层前缀账本**（决策一）：四阶段 × 两条路径（`internal/agent/context` 记忆路径 / `internal/agent/fsm` 降级路径）消息顺序统一为 L0 稳定核（阶段契约 + `ImmutableCore.StableMessage`）→ L1 会话层（核心记忆、子 Agent 画像、可信工作区指令、扩展目录、不可信工作区上下文，`fsm.WriteSessionLayer`）→ L2 历史层 → L3 阶段层（阶段模板/选择器 + 压力提示 + 工具目录/Plan 提示块 + `ImmutableCore.VolatileContent`）→ L4 回合层（召回、TaskModel、GroundingGap、观测、重规划反馈、执行结果、本轮意图）。`PromptBuilder.BuildLayered` 只改输出次序，Zone 信任分区与围栏不变。字节稳定规则：L0/L1 内集合必须确定序、不得含时间/连接状态（MCP 连接状态在 L3）。门控：`internal/agent/context/prefix_ledger_test.go`、`internal/agent/fsm/prompt_layers_test.go`。
- **阶段契约并入 L0**（决策九，`m4_kernel.prompt.phase_contracts_in_core`，默认开）：L0 由两条 system 组成，按稳定度由高到低——①`# PHASE CONTRACTS` 段（`configs/phase_contracts.go`，四阶段正文即 `kernel/<phase>.md`，字节恒定、不计入 `maxSystemPromptBytes`）；②可变核心（`ImmutableCore.StableMessage()`：身份/模型指引/自定义指令/平台提示 → 运营指令 → 工具提示 → 用户画像 → 偏好按键排序；超 32KB 只从尾部截断）。仅内核经 `StableMessagesWithContracts()` 取 `[契约段, 可变核心]`（多于一条时首尾均带 `CacheBreakpoint`）；网关直连/cron/workflow 的 `StableMessage()` 不含契约段。各阶段 L3 由 `fsm.WritePhaseContract` 写选择器（`# ACTIVE PHASE: <NAME>`）；开关关闭、降级路径或无契约能力时回退写完整模板。
- **对话历史进内核**（决策二，ADR-0098 决策四）：`AgentController.SetConversationHistory` 由 session 在 `SetTaskIntent` 前注入（剔除 system）；四阶段共用同一 L2，逐条以真实 user/assistant 角色写入，每条独立 TaintHigh 围栏（标记按单条内容哈希）。`fsm.WindowConversationHistory` 分块跳窗：超 `thresholds.m4_kernel.conversation.history_max_messages/bytes` 时按 max/2 步长一次丢前若干块，被丢部分以一条确定性锚定摘要（条数 + 最近 8 条首行截断，无 LLM）置于 L2 首位；切点是历史的纯函数，跳窗之间 L2 纯追加。不带 `ReasoningContent`。
- **召回单一管线**（决策四 + 决策十）：全内核一条管线、一处注入点（L4 首位），`injectMemoryToMsgs`/`Assembler` 第二管线已删。各来源（反思/情景/L2 语义/RAG）先按原分降序，再 RRF（k=60，`score=Σ weight_source/(60+rank)`）；同正文跨来源累加只留一条。权重 `m4_kernel.recall.weight_*`（反思 > 情景 > L2 = RAG，0=关闭该来源）。按融合名次装到 `max_tokens`，渲染按来源分组、组序固定，输出确定。预算与阈值见 `spec/state.yaml §m4_kernel.recall`。用户画像不在召回里，仅走 L0 `ImmutableCore.UserProfile`。
  - 相关度门：`min_score_ratio` 默认 0；注入 `StateContext.RecallReranker` 时对融合前 top-N 取相关概率，丢弃低于 `recall_rerank_min_prob` 者；失败一律跳过门。当前生产无重排器加载路径（`ffi.LlamaRerank` 是单槽位 bi-encoder 余弦，非 cross-encoder），门在生产中不生效，仅保留消费端接口与 `SetRecallReranker` 注入点。
  - 迁自 Assembler：SurpriseIndex 低于 `recall.rag_min_surprise` 本回合不查 RAG，高于 `surprise_hint_threshold` topK 加倍；MaxTaint 过滤取意图 `OriginTaintLevel`（None 按 High，fail-closed），条目逐条带 `Taint`，超限丢弃。
  - L2 语义召回与情景相关度召回：`cmd/polaris/adapters_recall.go` 的 `recallCognitiveAdapter` 以 FTS 命中 `sement_` 语义实体与情景事件（BM25，项目隔离经 `projectScopedFTS`/`projectScopedEpisodicFTS`），按 ID 取正文并再判项目与污点（fail-closed）；向量路不接；Tier0 无 SurrealDB 或 FTS 失败时来源为空，不阻断回合。RAG 分取检索器真实分。
  - 回合内复用：`StateContext.TurnRecall`（分来源条目 + `EpisodicDone/GoalDone` 标记 + 渲染文本）在 IDLE→PERCEIVE 与 `PreparedReply` 同处清空；Perceive 只查情景（按本轮原话），反思/L2/RAG 以 Goal 为查询词，一律留给 Plan 用本回合 Goal 补查；读写 `TurnRecall` 须持 `sCtx.Mu`，召回 goroutine 不得触碰 sCtx。
  - 去重：正文（规范化空白后 ≥12 字符）被当前 L2 历史包含则融合前丢弃。Reflect/Respond 的执行结果经 `fsm.ExecuteResultForPrompt` 收敛到 `ObservationMaxBytes`（优先取带 `read_tool_ref` 提示的最近观察，污点告警原样保留）。
- **回复上下文**：L3 为 `kernel/respond.md`，L4 为（执行路径）目标/执行结果/反思（TaintMedium 起）+ 本轮意图压轴；无 tools。`internal/agent/context/respond_context.go`。
- **真实请求边界门控**（决策十一）：`internal/agent/request_prefix_gate_test.go` 以录制式 Provider 捕获实际发出的消息，断言同回合 L0..L2 字节一致、相邻回合前缀关系、L0 全程一致、tools 同回合一致且跨回合只追加、无 `Relevant Context:` 旁路、同一 PII 同一令牌；覆盖令牌化、热路径压缩（仅压缩缓存前缀之后）、溢出恢复、PRM 候选。PII 令牌映射按会话生命周期存活（`TokenVault.ReleaseTask`，回合终态只释放不清空）。

---

## 2. Suspend-on-Idle Actor

Agent 以 goroutine 运行，空闲时挂起释放资源。

**EpochTracker（上下文指纹隔离）**: `internal/agent/fsm/epoch.go` 的 `epochTracker` 对每次 LLM 调用前的消息序列计算 SHA-256 全量指纹；指纹变化时 epoch 原子自增，调用方（`state_machine.go`）比较 `StateContext.ContextEpoch` 判断上下文是否失效。

**运行循环**: 等待 intent channel 脉冲 → 推进状态机 → 处理 LLM/工具 events → 空闲超过 SuspendIdleThreshold（`spec/state.yaml §m4_kernel.suspend_idle_threshold_minutes`）自动 checkpoint 到 SurrealDB-Core KV 后释放 goroutine。HITL 等待期间经 M2 EventLog Subscribe 监听 ApprovalResolved（非 Go channel，防进程崩溃丢审批）。

**咨询式挂起（提案，未实现）**：`AskHuman` 特权工具拟复用同一 Suspend/Resume 路径，新增 `SuspendReason=awaiting_user_input`，不新增状态。→ [ADR-0066](./decisions/ADR-0066-gateway-sql-sink-and-egress-hardening.md)（HITL AskUser，原 ADR-0042）。

**内存持久化失败熔断（GD-13-003）**：写入路径同步落盘，失败以同步 error 返回。`internal/agent/agent_execute_memory.go` 的 `writeEpisodicWithExtract` 捕获错误，`isMemoryPersistenceFailure` 以 `apperr.IsCode(err, apperr.CodeStorageUnavailable)` 判定存储层故障（业务校验错误不熔断）；命中后 `handleMemoryPersistenceFailure` 设 `StateContext.SuspendReason="memory_persistence_failure"`、写一条 `m9_storage_degraded` outbox 记录，并 `asyncIntent(types.TriggerInterruptReceived)`——复用 `Dispatch()` 中早于 transitions 表的全局中断处理器，路由至 `AgentStateInterrupt`（不是 `AgentStateSuspended`，未新增转换规则）。

**空闲挂起只在真正空闲时发生**（2026-10 修复）：`Run()` 的 `idleTimer` 触发时，仅当 FSM 处于 `S_IDLE` 且无 effect 在运行才 `Dispatch(TriggerSuspend)`；回合内（长耗时工具、等待人工审批——无人值守审批可长达 24 小时）一律重置计时器，effect 完成也重置。此前非 Idle 态下 `Dispatch` 失败后 `ForceState(Suspended)`，工具返回的 `TriggerLoopExecDone` 即报 `no transition from 11 with trigger 24`（11=S_SUSPENDED，24=TriggerLoopExecDone）。回归：`agent_idle_suspend_test.go`。

内存效率: 活跃 Agent 约 1MB，休眠 Agent 仅留约 100 字节 checkpoint 元数据。Tier 0 硬上限 2 个活跃 Agent。

---

## 3. S_VALIDATE 四层校验

**前置条件**：`DAGModel == nil`（FastPath 空执行路径）时，`executeEffect` 直接发 TriggerValidateOk，不进入校验。

**实现说明**：`StateMachine.validateDAG`/`executeDAG`（`internal/agent/fsm/state_machine_effects.go`）是委托存根（返回硬编码 OK）；真实校验在 `Agent.runValidateDAG`（`agent_execute_validate.go`）与 `Agent.runExecuteDAG`（`agent_execute_dag.go`，Saga 逆序补偿），由 `Agent.Run` 主循环调用。跳过 `Agent.Run` 直接 Dispatch 这两个事件将触发防御守卫返回错误。

| 层级 | 延迟 | 适用范围 | 检查内容 |
|------|------|---------|---------|
| L0 拓扑 | <1ms | 所有 DAG | 见 [L0 补充](#l0-补充) |
| L1 确定性 | <1ms | 所有动作 | 见 [L1 补充](#l1-补充) |
| L2 启发式 | <5ms | RiskHigh+ | 批量规模（>100）/ 受保护路径（`/etc/`,`/sys/`,`~/.ssh/` → 拒绝）/ 资源预估 vs Tier 阈值 |
| L3 LLM 看门狗 | ~200ms | 仅 RiskPrivileged | 见 [L3 补充](#l3-补充) |

#### L0 补充
节点数熔断 → DFS 三色环检测 → 深度熔断 → 悬空依赖；阈值 `spec/state.yaml §m4_kernel.plan_dag_max_nodes/max_depth`。"悬空依赖"（依赖引用未定义节点 → 拒绝；无依赖的独立节点合法）取代原"孤立节点"规则：后者与原生并行 tool_calls（`toolCallsToDAGJSON` 生成无边独立节点）冲突，且漏检真正的依赖写错（ADR-0098 决策五）。

#### L1 PolicyGate 请求形态（ADR-0098 决策五）
与执行闸门 `sandbox.ExecEnvelope` 同问：`(principal=agent, action=tool_execute, resource=<工具名>)` + `trust_tier/risk_level/tool_source`（经 ToolExecutor.Lookup）+ `capability_token_valid=false`（规划期未签发令牌）。词汇 SSoT：`internal/protocol/policy_vocab.go`。

#### L1 补充
TaintGate + JSON Schema 双向校验 + Tool availability + PolicyGate（Cedar-Gate FORBID 优先）。L1.1 资源冲突检测：规范 artifactID → 对无依赖边的并行写冲突节点自动注入隐式序列化边（EdgePrecondition），审计 `implicit_resource_edge`。

#### L3 补充
LLMWatchdog（仅 RiskPrivileged，不设频次上限）输出 ALLOW/DENY；L3 为补充信号，不可推翻 L0/L1/L2 拒绝；DENY → S_VALIDATE_FAIL，ALLOW 或 LLM 不可用 → fail-open 推进 S_VALIDATE_OK。

**ActiveTaintLevel**：按 DAG 节点 `pCtx.MaxTaintLevel` 传播，L1 TaintGate 对单节点污点拦截有效。

**TaintGate**（L1 第一道 — `[Taint-Prop]`）:
- **Layer A 上下文传播**: 非系统源 `[Taint-Medium]`+ 数据 → LLM 产出继承上下文最高 `[TaintLevel]`（系统自生成 source∈{compaction, persona_refinement, consolidation, skill_compilation} 排除）
- **Layer A.1 工具调用结构化降级**: DAGNode tool_call 通过 JSON Schema 校验（InputSchema + OutputSchema 双向）后，参数值可经 `[Taint-Sanitizer]` SanitizeBySchema 降一级；字符串字段仅当 schema 定义 format/pattern/enum/const 内容约束时降级（裸 `{"type":"string"}` 不降级，见 M11 §2.5）；TaintMedium 工具调用 → TaintLow 参数，允许写 workspace；自由文本 LLM 响应不受此规则影响；每次降级写 `taint_schema_downgrade` 审计事件。
- **Layer B 精确子串**: `taint_sensitive` 字段 vs active taint set
- **输入反序列化**: TaintedJSONNode 递归树（禁止 map[string]any，防 Go JSON 剥离污点标记）
- **TaintBlocked**: → HITL → TaintExemptionToken（field_hash+TTL）
- **SchemaValidator**: Taint 扫描 → InputSchema → OutputSchema 一致性 → 幂等 ID 合法性（`internal/execute/dag/validator.go` 仅校验结构，字段级降级在 M7 工具调用层）

PolicyGate: `[Cedar-Gate]` {principal, action, resource, context} → FORBID 优先。

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
| Context 隔离 | 共享父 Agent PromptBuilder | 见 [Context 隔离补充](#context-隔离补充) |

#### Context 隔离补充
**每个子 Agent 持有独立 PromptBuilder 实例与 context window**，仅通过 Blackboard 结构化 result entry 交换（[Sub-agent-Isolation]）。M8 派发 Macro-DAG 节点时为每个执行 Agent 创建独立 `prompt.PromptBuilder`（接口 `internal/protocol/prompt_builder.go`，实现 `internal/prompt/prompt_builder.go`），禁止共享父 Agent 的 ImmutableCore/MutableSkill/TaintedData zone；父 Agent 订阅 Blackboard 事件消费子结果（见 00-Global-Dictionary §9-bis）。

### 4.2 数据模型

DAGNode/DAGEdge/EdgePolarity/RetryPolicy/Compensation 类型见 `pkg/types/`（`DAGModel` 等）与 `internal/execute/dag/`。

**DAGModel 两种来源收敛为同一表示**：S_PLAN 的 LLM 产出有两条输入——① 文本 JSON DSL（符合 plan_dag Schema）；② Provider 原生 `tool_calls`（`types.WithTools` 随工具目录非空自动附加，`doStreamInfer` 累积 `StreamToolCall` 为 `ProviderResponse.ToolCalls`，经 `toolCallsToDAGJSON` 转为等价 DAGModel JSON，`internal/agent/agent_execute_effect.go`）。下游 S_VALIDATE 与 DAG Executor 不感知来源，无第二条执行路径。

### 4.3 DAG Executor

实现：`internal/execute/dag/`。执行流程:
0. M8 `Blackboard.BeginExecution(taskID, agentID)`：CAS Claimed→Executing（闭合 Pending→Claimed→Executing→Done/Failed 生命周期）
1. findReadyNodes：DependsOn ⊆ completedSet → 就绪，同批字典序优先
2. 副作用分类：read_only/pure → 并发；write_* 的回滚规则见下
3. LeaseHeartbeat goroutine：每 15s(±5s jitter) 续期，防 M8 Reaper 误判超时
4. errgroup 并发，sem channel 限并发度（`spec/state.yaml §m4_kernel.max_concurrent_nodes`）
5. 任意失败 → 已完成并行节点逆序 Undo 补偿
6. 全部完成 → 停止 Heartbeat

**回滚分级（2026-09-20 裁决，废止"write_* 节点必须声明 CompensationAction"）**：plan_dag schema 与 plan prompt 不产出 compensation 字段，强制校验会使含写操作的计划 100% 无法通过 S_VALIDATE，且与 inv_M4_06 冲突。
- write_local：回滚由 VFS 快照 + Git revert 承担（确定性、不依赖 LLM 声明）
- write_network / privileged：由 S_VALIDATE BlindZoneHITL 拦截（inv_M4_06）
- CompensationAction 仅保留给 Macro-DAG（跨 Agent 编排的人工预定义静态补偿步骤）

### 4.4 Dynamic DAG Replanning

节点输出 `[SurpriseIndex]` >0.7 → 未执行下游子图局部重规划。已成功节点保留（防双重副作用）；必须覆盖已执行节点时，先 Saga Compensation 成功才加入 replan。重规划在 S_EXECUTE 内部，不跨状态机边界。

### 4.5 StepScorer + Adaptive Max-Steps

实现：`internal/agent/step_scorer.go`、`internal/agent/step_scorer_prm.go`。

- **Tier 0 纯静态启发式**: 权重 toolSuccess=0.4, schemaCheck=0.3, latency=0.2, tokenEfficiency=0.1；Score 从 1.0 起按四项扣分，latency/token 惩罚封顶。
- **Tier 1+ 启发式 + PRM 融合**：复用 `Agent.provider`——仅当其实现 `protocol.LocalProvider` 且 `HardwareTier() >= Tier1`（`newStepScorer()` 构造时判定）才挂载 `stepScorer.prm`，否则等价 Tier 0。经 GBNF 约束（`root ::= "+1" | "0" | "-1"`）三态离散打分；融合 `(1-0.6)*static + 0.6*prmNormalized`（+1→1.0/0→0.5/-1→0.0）；100ms 硬超时（独立于调用方 ctx）；FFI 错误（含 Rust `catch_unwind` 的 OOM/panic）与非法输出一律降级为纯静态分，不抛 error。不新增独立 PRM 模型常驻通道（单槽位 FFI 协议限制）。调用点 `internal/agent/agent_execute_dag.go`（工具调用后打分）；测试 `internal/agent/step_scorer_prm_test.go`。

**Adaptive Max-Steps**:
- `StateContext` 持有 `StepsUsed / MaxStepsLimit`；`AgentConfig.MaxSteps` 首次 `Run()` 写入 `MaxStepsLimit`（0=无上限，不推荐生产）。
- `Agent.Run()` 每轮 trigger 前计步：`StepsUsed > MaxStepsLimit` → FSM 熔断 `S_FAILED`，错误码 `MAX_STEPS_EXCEEDED`。
- 每次工具调用后 `adjustMaxSteps(current, score)`：score < 0.5 → 收紧 10%；≥ 0.5 → 不变。`StateContext.InitialMaxStepsLimit` 记录初始值，每次 S_PLAN 生成新 DAG 时重置，惩罚仅在当前 DAG 周期内生效，不跨任务累积。

**Best-of-N 与 Replanning 阻断**：低分分支标记为 MEMF 失败候选池，累积低于警戒线立即触发重规划或 Saga 补偿。

### 4.6 ProcessRewardModel — S_PLAN 候选 DAG 选优

PRM 在 **S_PLAN** 对多个候选 DAG 并发打分选优；与 §4.5 StepScorer（执行中单步评分）职责不同。

**实现状态（2026-10-12 按代码订正，推翻 2026-07-28"FSM 未接线"裁决）**：已接线但默认关闭。`cmd/polaris/boot_agent.go` 在 `m4_kernel.prm.enabled`（默认 false，`spec/state.yaml §m4_kernel`）为真时 `InjectPRM(NewDefaultPRM(...))`；多候选分支位于 LLM Effect 执行内（`internal/agent/agent_execute_effect.go`，非 FSM 转移）：`ShouldActivate(Complexity)`（复杂度 ≥ `prm.complexity_gate`）后并发生成 `prm.max_candidates` 份候选（各自过 schema 校验，失败者作废），`DefaultPRM.SelectBest`（`internal/agent/prm.go`）用 budget-tier LLM 打 0–1 分取最高；全部低于 `prm.min_threshold` 或出错 → 取第一个候选；全部候选失败 → 降级单次 Infer。候选 token 计入 `TokensUsed`。默认关闭的原因：每次 S_PLAN 的 LLM 调用量 N+N，与 Tier-0 预算纪律（`ARCHITECTURE.md §1`）冲突；默认开启须先有 Eval 证据（成功率增益 > token 成本增幅）并新立 ADR。`SelectBest` 亦被 ADR-0048 PRM 训练样本采集复用。

**DAG 执行结果聚合**：S_EXECUTE 完成后统一收集节点输出；单节点取 output，多节点序列化为结构化结果集，供 S_REFLECT 访问完整上下文。

### 4.7 spawn_planner — 后台子规划器

`internal/swarm/planner/`（`TaskDecomposer` + `AgentPool`）属 **M08 Swarm 层**的 Macro-DAG 分解能力，不是 M04 内部组件——`internal/agent` 不 import 该包，单 Agent FSM 的 S_PLAN 不直接触发该池。本节仅标示复杂任务从单 Agent 规划溢出到 Swarm 的链路；设计见 `M08-Multi-Agent-Orchestrator.md §5`。

---

## 5. System 1/2 双轨路由

`[HE-Rule-5]` System 1 物理边界：零 LLM 调用，100% 本地 Python/Go 技能（ContainerSandbox）+ SurrealDB-Core KV 缓存；未命中 → 无条件升级 System 1.5。FastPath 跳过 LLM 但仍执行确定性记忆写入（Episodic + Outbox）。

| 路径 | `[SurpriseIndex]` | 延迟 | 模型来源 |
|------|-------------------|------|---------|
| System 1 | <0.3 | 亚毫秒 | L0 技能缓存 (零 LLM) |
| System 1.5 | 0.3-0.6 | 毫秒-秒 | M1 Budget Pool |
| System 2 | ≥0.6 | 秒级 | M1 Reasoning Pool |

**`SelectPlanTier` 注入**（与 System 路由正交）：`internal/agent/fsm/transitions_respond.go` 的 `planEffect` 在 LLM 调用前调 `SelectPlanTier(sCtx.Escalation, TaskModel.Complexity, SurpriseIndex)` 得规划模型池与 `[ThinkingMode]`，经 `types.WithModelPool` / `protocol.WithThinkingMode` 传入。`sCtx.Escalation` 只由能力类失败累计（`fsm.RecordFailure`：S_VALIDATE 按层 `ClassifyValidationLayer`、执行按错误码 `executionFailureKind`、`tryEscalatePlan`）。阶梯与 Provider 映射见 M1 §5.2-bis。

**SurpriseIndex 来源（ADR-0025 BUG-D）**：`populateSessionContext` 优先读注入的 `SurpriseReader`（`learning/surprise.SurpriseCalculator`，Jaccard + MEMF + Markov）；未注入退回 `ComputeBasic`（仅 Jaccard）；结果写入 `metrics.GlobalSurpriseIndex`。`SurpriseReader` 为 consumer-side 接口（防 L1→L2 包循环）。`SurpriseCalculator` 为**进程单例**（cmd/polaris 构造一次注入全部 Agent，关停时 Close；ADR-0104 决策七），结果按 taskID 分桶（有界 LRU 1024），启动时 Markov 矩阵由 `session_trajectory` 最近 500 个会话工具序列 warm-start。

RouteReasoning:
0. si = `Agent.surpriseCalc.CurrentSurprise(taskID)` 或退化路径 `ComputeBasic(nil, toolSeq)`；均不可用 → 0.5。**`si=0` 为默认零值，不触发 FastPath。**
1. `0 < si < 0.3` → FastPath：合成 S_PERCEIVE 结果跳过 LLM，S_PLAN 同样旁路；skillCache 命中直接执行 Wasm，不兼容 fall through。
2. 未命中或 si ≥ 0.3 → 走 Plan（`providerRouter.Route`）。工具选择：原 `M6.SkillSelector.SelectTopK` 已废弃，由 M13-bis CompositeCatalog 懒加载 + `tool_search` 取代（工具总数超 LazyLoadThreshold 时仅常驻核心集，其余在系统提示里留一行索引，LLM 经 `tool_search` 关键词/BM25 检索并激活，不依赖 Embedder，覆盖 builtin + MCP + skill，见 M13-bis §4 与 ADR-0119）；System 1 意图匹配由 `SkillIntentMatcher` 独立承担（`cmd/polaris/boot_agent.go`）。**Tool Selection > Tool Design**：禁止把全部工具列表塞给 LLM。
3. buildMessages：见 §1.1 五层前缀账本。

**AgentPool（ADR-0025 §E）**：`ChatHandler` 持有 `AgentPool`（consumer-side 接口）；每个 sessionID 一个独立 `Agent`，容量由 `TierParams.MaxConcurrentAgents` 限制；超容量 Acquire 等待 100ms 后返回 `CodeResourceExhausted`。idle 超 10 分钟由 `Pool.GC()` 回收（调用 `Shutdown()` 停循环）。`Pool.Acquire` 对新建 Agent 立即以其自身生命周期 ctx 启动常驻 `Run()` 事件循环（实现 `internal/agent/pool.go`）。

---

## 6. World Model

L1 快速路径：SurpriseIndex < 0.3 旁路 LLM（`agent_execute_effect.go` FastPath 分支）。L2：LLM 执行后 SurpriseIndex 更新并影响下一轮路由。StatePredictor / Isotonic Regression 已废弃，路由完全由 SurpriseIndex 主导。

**知识空缺感知**：LLM 推理前调 `WorldModel.AssessGrounding` 评估上下文充分性；不足时将警告（GroundingGap，L4）注入 prompt 末尾，引导优先检索，不直接拦截。WorldModel 实现见 `internal/memory/`；上下文组装由 `internal/prompt/` 的 `PromptBuilder` 统一执行（四 Zone + TaintData Spotlighting 门控）。

---

## 7. 推理预算管理

| 层级 | 粒度 | 机制 | 默认值 |
|------|------|------|--------|
| 思考步数 | 单次 DAG 推理步数 | MaxReasoningSteps | 5 |
| 思考 token | 单次 LLM reasoning | MaxThinkingTokens | 4096 |
| 任务预算 | 单次 Agent 任务 | TaskTokenBudget | 50K |
| Session 预算 | 单次 Session | SessionTokenBudget | 200K |

三模式: `fixed` (MaxReasoningSteps=5, MaxThinkingTokens=4096) / `adaptive` (`min(16384, 4096×(1+[SurpriseIndex]×3))`, 1000+ 样本后) / `batch` (32K, 夜间)

**BudgetManager**（`internal/agent/budget.go`，实现 `fsm.BudgetController`，经 `SetBudget()` 注入 `StateContext.Budget`）是唯一预算判断入口：
- **记账（ADR-0104 决策四）**：无内存计数，唯一账本为 `llm_calls` 表。`ConsumeTokens` 入参仅用于 `trace.RecordBudgetTokens` 埋点；会话用量读 `LLMSpendLedger.SessionTokens(sessionID)`（可滞后一次调用，软熔断可接受）；`MonthlySpendUSD()` 读当月（UTC）`cost_usd` 合计，与 `MonthlyLimitUSD()` 一同向 Cedar `budget_cap` 填 `monthly_spend_usd`/`monthly_budget_usd`。账本读失败 fail-open（Warn 放行）。内联 `StateContext.TokenBudget/TokensUsed` 保留作向后兼容，`Budget != nil` 时双轨并行。
- **注入与上限**：`buildAgent`（`cmd/polaris/boot_agent.go`）为每个 Agent 注入绑定自身 sessionID 的 BudgetManager；月度上限每次经 `BudgetRepository.GetBudget()` 读取（30s TTL 缓存），`HandleSetBudget` 只写库，无需推送。

### 7.0 任务级预算自适应截断

每次 DAG 节点循环入口三级检测（`internal/agent/`）:

| 阈值 | 条件 | 动作 |
|------|------|------|
| 50%（警告） | `TokensUsed * 100 / TokenBudget >= 50` | 首次写 `BudgetWarned=true`，`slog.Info`；不重复 |
| 75%（压迫） | `TokensUsed * 100 / TokenBudget >= 75` | 首次写 `BudgetPressure=true`，`slog.Warn`；触发 S_PLAN `[BUDGET_CONSTRAINT]` 注入 |
| 100%（硬熔断） | `TokensUsed > TokenBudget` | FSM 直接转 `S_FAILED`，错误码 `INFERENCE_OOM` |

**BudgetPressure → [BUDGET_CONSTRAINT]**（`promptPlan()`，`internal/agent/fsm/state_machine.go`）：`BudgetPressure=true` 时在 S_PLAN Prompt 追加系统指令 `[BUDGET_CONSTRAINT] Token budget > 75%. Generate a MINIMAL DAG: max 3 nodes, only strictly necessary tool calls, no exploratory steps. Remaining: N tokens.`；来源 `TaintNone`，经 `SanitizeToSafe` 注入 `WriteInstruction` slot。`BudgetWarned/BudgetPressure` 在重放时从 EventLog 恢复，保证 inv_M4_03。

**KillSwitch 联动**：M4 不重复实现 TokenBurnRate 检测，也不独立触发 KillSwitch 阶段变迁。TokenBurnRate CANONICAL SOURCE 是 M3（EMA_5s + EMA_30s），M3 直接推送至 M11 KillSwitch.CheckAndAct（M11 §4.3），这是阶段变迁的**唯一路径**。M4 每次 LLMFillEffect 前读 `observability.GlobalKillswitchStage`（原子 int32）并响应（`agent_execute_effect.go`）:
- **Throttle**: `sCtx.MaxStepsLimit` 收紧至 3，`sCtx.ThrottleNoNetwork = true`；DAGExecutor `toolExecFn` 对含 `SideNetworkCall` 副作用的工具返回 `ErrForbidden`
- **Pause**: 返回 CodeInternal 错误，挂起当前 LLM 调用，等待 M11 恢复或 ESCALATE
- **Fullstop**: 状态机推进 S_FAILED，拒绝新 Infer 调用

跨模块规则见 `00-Global-Dictionary.md` [XR-01]。

**ContextWindowManager**（热路径上下文管理，默认 maxTokens=90000，`InjectContextWindowManager` 可覆盖；与 M5 SessionCompressor 冷路径协同，实现 `internal/agent/agent_context_compaction.go`）:
- >70% → salience 排序，底 30% 候选交 M5 SessionCompressor 压缩（M5 §11）
- >90% → 语义结构感知逐出（以完整 DAGNode/tool_result/Episodic Event 为单位）
- 仍超限 → 触发 M5 Consolidation 全量压缩（M5 §9）

M4 仅持有热路径窗口管理与触发判断；压缩算法、锚定策略、cold path 委托 M5（Compaction as First-Class，单一权威源）。

### 7.1 `[ReasoningState]` 跨轮持久化

`StateContext.LastReasoningContent` 持有本轮 `ProviderResponse.ReasoningContent`（Adapter 提取，`agent_execute_effect.go` 存入 `sCtx`）；下次构建 messages 时作为 assistant 消息的 `reasoning_content` 回传，满足 DeepSeek 多轮工具调用 API 约束。同回合 tool_call 往返有效；跨 session/跨任务不继承，会话历史不带该字段（§1.1 对话历史）。`FeatureReasoningStateCarry`（Tier 1+）扩展至 episodic_events 落盘持久化（msgpack + AES-256-GCM，SessionPIIVault.SecureZero 同步清零）。

---

## 8. Crash Recovery

满足 [HE-Rule-6]（State-in-DB）：崩溃恢复从 M2 EventLog / 轨迹表回放，不依赖独立 FSM checkpoint 文件。状态持久化由 `runFSM` 在每次转移完成后显式触发（`internal/agent/fsm/transitions.go`），Transition.Effects 回调本身不持久化。Suspend-on-Idle 的 KV checkpoint（§2）仅用于释放 goroutine 栈，非崩溃恢复路径。

**回放机制**: M4、M5、M11 启动时统一检查 [ReplayMode]（进程级 atomic.Bool，`internal/protocol/replay.go`）。回放期间禁止所有外部副作用（EmitEvent/ToolCall/Outbox），纯函数式重建；追平后退出回放，从崩溃点继续。

**会话崩溃恢复（ADR-0104，原 ADR-0076 决策一/二/四）**:
- `Agent.Run()` 写 `inflight:session:{id}` KV 标记，干净退出 `defer` 清除；进程崩溃则残留。`recoverCrashedSessions`（`cmd/polaris/boot_crash_recovery.go`，`bootAgent` 后 `bootServer` 前）扫描候选会话，用 `TrajectoryRecorderImpl` 从 `session_trajectory` 表（045）重建 `TrajectoryTrace`，**仅当最后状态落在纯 LLM 状态（S_PERCEIVE/S_PLAN/S_REFLECT）时自动恢复**；S_VALIDATE/S_EXECUTE/S_REPLAN/S_ROLLBACK 保守跳过（2PC 预写日志未经专门审计）。经 `AgentPool.Acquire` + `InjectReplayData` 注入历史 LLM 录像 + `SetReplayMode(true)` 驱动续跑，消费最后一条录像的同一瞬间翻转为 false；工具执行在 ReplayMode 窗口内短路为 stub。
- agentic 回合续跑（`recoverAgenticLoopTurns`）重放前经 `RepairDanglingToolCalls` 整段修复历史：任一 assistant.tool_use 缺 tool_result → 在其结果段内补“执行中断，结果未知”（先于其后的 steer/user 消息）；孤儿 tool_result 降级为 user 文本。否则 DeepSeek/OpenAI 兼容协议返回 400（assistant.tool_calls 后缺 tool 消息）。`ResumeAgenticLoop` 再做一次纵深防御。
- 工作流级：`task_checkpoints`（`(task_id,node_id,attempt)`）命中 `done` 节点直接复用；`POST /v1/workflows/runs/{id}/resume` 以同 runID 手动续跑，启动期不自动续跑（ADR-0104 决策二）。
- 启动期孤儿对账：`cmd/polaris/boot_orphan_reconciler.go`（ADR-0104 决策一）。
- S_AWAIT_AGENT 被动恢复：见 §8-bis。

**双重幂等防线**: ① isReplaying 标志物理切断副作用；② UNIQUE(session_id, seq) 约束 + idempotency_key 保证重复事件幂等消费。

**Replay Key（录像 key）**: EventLog 事件 ID 为 `{session_id}:{seq}:{event_type}`，seq 单调递增，同 session+seq 始终同 ID，不依赖 wall clock。回放时 event_type+ID 与 FSM PromptFn 占位符对应缺失 → 触发 `g_inv_08`（禁止重新调 LLM，进入 REPLAY_MISMATCH 审计）。非重放路径上 `uuid.New().String()` 生成的 2PC 中间事件靠 idempotency_key 防重入，回放时被 `isReplaying=true` 切断。

**长任务快照（规范保留，代码未实现）**：设计为 >5min 任务每 5min 或每 10K 推理 token 写 SurrealDB-Core KV `session_snapshots`（key=`{session_id}:{seq}`，TTL 24h，per session 上限 5），events 表记 `source='snapshot_checkpoint'`，恢复优先加载最近快照再差量回放，损坏回退 EventLog 全量回放。步频/保留数 `spec/state.yaml §m4_kernel.snapshot_interval_steps` / `snapshot_retention_count`。2026-10-12 `rg` 核实：仓内无 `session_snapshots`/`SnapshotContext` 实现，仅 `internal/config/thresholds.go` 保有配置项。落地前，崩溃恢复以上述轨迹回放为准。

**PII**: 快照/日志不含明文——ToolResult 经 M7 §4.3 Step 5 PostExecution Redact 后写 EventLog；恢复需 SessionPIIVault 仍存活以解析 token。

**S_REPLAN 降级**: M1 CircuitBreaker 熔断时执行零 LLM 的确定性图剪枝（纯 Go 图遍历）：移除失败节点及其所有直接后继，注入 degraded_replan 标记；纯函数、幂等、可重放。

**S_REPLAN 扩展激活降级**（ADR-0098，原 ADR-0065；与 CircuitBreaker 剪枝为不同触发路径）：按需扩展激活（`FindAndActivate`）超时/失败时有限重试（预算封顶 `replanExtActivationTimeout`，默认 3s）；仍耗尽 → 会话上下文置 `ReplanExtActivationDegraded` + `GlobalReplanExtActivationDegradedTotal` 计数，仍照常 dispatch `TriggerReplanDone`（不新增状态）。下游 DAG 遇 `capability_gap` 且该标记为真 → 直接快速失败并给明确原因。

**`ErrAllProvidersFailed`（全 Provider 熔断）**:
1. 确定性图剪枝后检查剩余节点：(a) 有 System 1 可执行节点（SurpriseIndex <0.3，零 LLM）→ 继续执行，**不消耗 ReplanCount**；(b) 全部需 LLM → **不消耗 ReplanCount**，转 `Suspended(suspend_reason=provider_exhausted, provider_suspended_count++)`，Blackboard 写标记，`SessionPIIVault.Snapshot(ctx, taskID, fields)` 把 PII 加密落 `task_pii_vault` 表（046，ADR-0104 决策八）。
2. `provider_suspended_count > 5` → 终止自动唤醒，`[ESCALATE]` + HITL。
3. 剪枝后剩余 DAG 为空 → `[ESCALATE]` 人工审批。

**Provider 恢复唤醒**：M1 CircuitBreaker Open→Closed → M2 Outbox 投递（`internal/llm/router.go` 发 `target_engine:"provider_recovery"`、`event_type:"m4_provider_recovery"`）。Handler `ProviderRecoveryHandler`（`internal/agent/recovery.go`）注册于 M2 全局 Outbox Worker（`internal/store/outbox_worker.go`），不在 M4 内另设 Worker（M2 §2.3 单写者）。序列：① 扫描 M8 Blackboard 全部 `suspend_reason=provider_exhausted` 任务；② `SessionPIIVault.RestoreFromSnapshot` 校验 `task_pii_vault` 快照仍在（缺失仅告警；意图取自 tasks 表，需明文时 `Load(ctx, taskID)`）；③ `Blackboard.ResumeFromSuspended(taskID)` Suspended→Pending；④ M8 ListenLoop 重新认领。

**FSM 终态 PII 清零**: 转 S_FAILED / S_COMPLETE 时，先于 WorkspaceManager GC 调 `SessionPIIVault.SecureZero(ctx, taskID)`，`task_pii_vault` 行先于 workspace 删除（GDPR 主动擦除）。

**Saga 补偿**: 确定性函数 + 预定义 HTTP 模板，禁止 LLM 参与。补偿前 M11 PolicyGate.Review 预检——FORBID → `[ESCALATE]` + `compensation_blocked_by_policy_revocation` 审计；非权限型失败重试 3 次（exponential backoff）。Micro-DAG 的 CompensationAction 字段保留但不强制（§4.3）；Macro-DAG（`state_graph_saga.go` / `pipeline_compensation.go`）的人工预定义 compensation_tool / compensation_args 继续有效（→ ADR-0088）。

完整时序见 `DIAGRAMS.md#eventlog`。

## 8-bis. Handoff 唤醒事件化 + 无损续跑（ADR-0104，原 ADR-0086）

`transfer_to_agent` 委派子任务后父 Agent 进入 `S_AWAIT_AGENT`。唤醒以 Blackboard 事件订阅（`bb.Subscribe`，`task_completed`/`task_failed`）为主路径；`handoffWatchSafetyInterval`（2min）巡检覆盖订阅静默失活；`handoffWatchPollInterval`（1s）纯轮询仅作 `Subscribe` 不可用的降级。

崩溃重启：`task_checkpoints.resume_ctx_json` 持久化 `HandoffResumeContext{SchemaVersion, DAGModel, ExecuteResult, CompletedNodeIDs, GlobalTaintLevel, HandoffNodeID, NamespaceID, SnapshotAt}`（上限 `handoffSnapshotMaxBytes`=256KB，超限降级为仅消除死锁）。`AwaitingHandoffReconciler` 扫描 `await_agent` 态 checkpoint，`ResumeAwaitingHandoff` 就位 FSM 后（快照可用且 `SchemaVersion` 匹配）**强制重跑与正常路径相同的 S_VALIDATE 四层校验**才回填 `DAGModel`/`ExecuteResult`/`CompletedNodeIDs`——`resume_ctx_json` 来自数据库，属跨信任边界反序列化输入，不因"自家表"放行（HE-2）。`GlobalTaintLevel` 回填 only-up。校验失败 / `dagValidator` 为 nil / 快照为空一律降级为"仅消除死锁"（委派节点下游不再续跑，但不误判为正常完成；`restored=false` 记 Warn + `RecordAgentHandoffResume(ctx,"degraded")`）。FSM 退出 S_AWAIT_AGENT 进入 S_EXECUTE 时清理该 checkpoint（`status="done"`），保证扫描幂等。

实现：`internal/agent/agent_handoff.go`、`internal/agent/agent_handoff_resume.go`、`internal/agent/reconciler_handoff.go`；启动调度 `cmd/polaris/boot_handoff_reconciler.go`。

---

## 12. 已知 Bug 修复记录

过程性修复记录已清理（历史见 git log）。仍具约束力的一条结论：原生 `tool_calls` 与文本 JSON DSL 必须收敛为同一 DAGModel（§4.2），禁止另建第二条执行路径。

---

## 13. 降级与失败模式（5 问全覆盖）

| 故障 | (Q1) 检测 | (Q2) 影响范围 | (Q3) 即时反应 | (Q4) 自动恢复 | (Q5) 人工介入触发 |
|------|----------|------------|------------|------------|----------------|
| LLM Fill 多次重试失败 | retry counter ≥ MaxRetry | 单 Agent 当前状态转移 | OnFailure callback → s_failed | 部分（下层 M1 CB 恢复后重试） | s_failed 进 audit |
| DAG 节点执行失败（可逆） | tool error | 单 step | step retry with backoff → 仍失败 → Saga 逆序补偿 → s_rollback | 是 | — |
| DAG 节点执行失败（不可逆） | Reversible=false + error | 单 Agent | s_failed + HITL 告警 | 否 | 必须 HITL |
| StructOutput JSON 解析失败 | JSON Repair 失败 | 单次 LLM 调用 | retry (1 次) → 仍失败 → s_replan | 是 | 同模型连续 ≥10 次 → audit |
| ReplanGuard 超限 | ReplanCount > MaxReplanAttempts (`§m4_kernel.max_replan_attempts`) | 单 Agent | 经 VALIDATE/ROLLBACK/REFLECT → s_respond（TurnDegraded）；其余路径 s_failed + HITL 告警 | 否 | 必须 HITL |
| DAG 死锁（无就绪节点） | findReadyNodes 返回空且未完成节点 > 0 | 单 Agent | ErrDAGDeadlock → s_failed + EventLog | 否 | M12 复盘 |
| Agent goroutine panic | recover() | 单 Agent | Supervisor OneForOne 自动重启 + EventLog 回放 | 是 (100ms→30s, 5 次上限) | 同 Agent 反复 panic ≥3/min → escalate |
| HITL 审批超时 | deadline 到期 | 单 Agent | s_rollback（不触发 KillSwitch，仅当前任务失败） | 用户重新发起 | 反复 expire → audit |
| 进程崩溃 | exit | 全局 | 重启后按 §8 恢复，不重调 LLM | 是 | — |
| Replay key 漂移 | hash mismatch | 单会话重放 | 走 LLM 重新调用 + audit | 是 | 频繁漂移 → M12 排查 |

与 OSMemoryGuard 协同：ResourceGovernor.Admit() 在 Agent 启动前校验可用内存 + CPU；OSMemoryGuard 触发 L3 临界 → 仅保留执行中的 Agent，禁止新唤醒。

## 14. 跨模块契约

> 接口签名权威源在 `internal/protocol/interfaces_*.go`（按域拆分）+ `types.go`。本表仅列依赖方向 + 语义 + 锚点。

| 方向 | 接口/契约 | 用途 / 锚点 |
|------|----------|-------------|
| M4→M1 | Provider.Infer / StreamInfer | LLM 推理；SurpriseIndex consumer。M1 §2, §4 |
| M4→M2 | EventLog Append / GetEvents | 崩溃恢复回放真相源。M2 §2.1 |
| M4→M2 | Outbox `m4_provider_recovery` handler | `internal/store/` 注册；实现 `internal/agent/recovery.go`。M2 §2.5, M4 §8 |
| M4→M3 | OTel spans + SurpriseIndex 消费 | 双层回退 完整版→基础版→0.5。`[HE-Rule-1]` M3 §4 |
| M4→M5 | PromptBuilder / HybridRetriever | 记忆检索 + 上下文组装（Zone 布局 ZoneImmutable→ZoneCoreMemory→ZoneMutableSkill→ZoneTaintedData 四固定 Zone + `ZoneExternalCatalog` 信任分区，见 M11 §2.1）。M5 §2, §7 |
| M4→M6 | SkillLookup / SkillRegister | System 1 技能缓存 + Persona 兼容性。M6 §3, §4.3 |
| M4→M7 | ToolRegistry.ExecuteTool | S_EXECUTE 节点调用 `[Wasm-Sandbox]`。M7 §3 |
| M4→M11 | TaintGate / PolicyGate / KillSwitch | 查阅，仅响应不主动触发。M11 §2, §4 |
| M4→M11 | SessionPIIVault | Suspend 时落 task_pii_vault 表；Restore/SecureZero 跟随 FSM 终态。M11 §5.1 |
| M8→M4 | Blackboard.CAS Claim / LeaseHeartbeat | 多 Agent 调度入口。M8 §1 |
| Schema | AgentState 枚举、DDL 001_events / 003_episodic_memory / 007_tasks（含 suspend_reason / provider_suspended_count） | `pkg/types/enums_agent.go`, `internal/protocol/schema/` |
| 全局字典 | HE-Rule-5 状态机控制流、XR-01 | 00-Global-Dictionary §1-bis, §1-ter |
| 时序图 | EventLog 回放、KillSwitch 响应链 | DIAGRAMS.md#eventlog, #killswitch |

`[Module-Topology]` `[Code-Package-Mapping]`

---

## 15. 默认参数

完整阈值与重评触发条件: `spec/state.yaml §thresholds.m4_kernel`。
