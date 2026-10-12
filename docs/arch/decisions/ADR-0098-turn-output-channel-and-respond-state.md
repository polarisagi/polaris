# ADR-0098: Agent 回合控制流：输出通道分离、S_RESPOND、重规划闭环与扩展激活降级

> 状态：Accepted ｜ 首次：2026-09-25 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0065 ｜ 模块：M4 / M13（gateway/session）/ `internal/protocol` / `pkg/types`

结论：内部阶段与用户回复物理分通道；S_RESPOND（第 14 态）是回合唯一的用户回复出口、Complete 的唯一前驱；Perceive 产出结构化路由字段，由 Go FSM 决定直答还是规划执行。

## 旧引用映射

本文沿用原 ADR-0098 决策一~十编号（代码注释按此引用，不变）；原 ADR-0065 并入决策十一。

| 旧引用 | 新节 |
|---|---|
| 0098 决策一~十 | 决策一~十（同号） |
| 0065 全文 / A-3 | 决策十一 |

## 决策一：输出受众（Audience）由 Effect 声明，零值 fail-closed（原 ADR-0098 决策一）

- `protocol.LLMFillEffect.Audience`：`AudienceInternal`（零值）/ `AudienceUser`。只有 `AudienceUser` 的 token 才以 `AgentStreamEventToken` 发布；内部 Effect 的 token 只累积进 `resp.Content` 供 OnSuccess 解析。
- 零值即 Internal：新增阶段忘记声明时默认不外泄。
- 思考链（`AgentStreamEventThinking`）不受 Audience 约束，前端独立渲染为"思考过程"，不进回复正文、不落消息历史。
- 阶段进度以结构化 `AgentStreamEventPhase`（Content=`perceive|plan|execute|reflect|respond`）发布，客户端自行本地化；禁止用自然语言状态串承载阶段语义（R1.6）。

## 决策二：S_RESPOND 为第 14 态，是 Complete 的唯一前驱（原 ADR-0098 决策二）

| From | Trigger | To | 条件 |
|---|---|---|---|
| S_PERCEIVE | TriggerRespondReady | S_RESPOND | `NeedsTools == false`（直答；ADR-0105 决策十 4b′ 亦经此入边） |
| S_PLAN | TriggerRespondReady | S_RESPOND | 解析成功且 DAG 为空（含 FastPath 无缓存 DAG；`S_PLAN_EMPTY`） |
| S_REFLECT | TriggerReflectDone | S_RESPOND | 原 → S_COMPLETE 改指向 |
| S_RESPOND | TriggerRespondDone | S_COMPLETE | 回复非空 |
| S_RESPOND | TriggerFillRetry | S_RESPOND | 空回复且未超 MaxRetry（自环，未推出 token；决策七） |
| S_RESPOND | TriggerReplanExhausted | S_FAILED | 推理失败 / 重试后仍空回复（A-01），原因以错误事件发布 |

- 枚举值追加在尾部（`AgentStateRespond` / `TriggerRespondReady` / `TriggerRespondDone`）：状态以 `%d` 落盘 EventLog，崩溃恢复白名单按值匹配，插入中间会使历史事件错位。
- S_RESPOND 是纯 LLM 协处理器态、无外部副作用，列入崩溃恢复重驱白名单。
- 回复 prompt 输入：ImmutableCore + `kernel/respond.md` + 有界对话历史 + 本轮用户消息 +（若执行过）任务目标、执行结果、反思结论；执行结果与反思按 TaintMedium 起、对话历史按 TaintHigh 进 UserData 围栏。

## 决策三：阶段输出契约单源化 + 约束解码（原 ADR-0098 决策三）

- `configs/prompts/kernel/{perceive,plan,reflect,respond}.md` 是阶段契约唯一来源，记忆路径与降级路径都加载（渲染位置见 ADR-0105 决策三）。
- Perceive / Reflect 请求 `json_object` 约束解码；Plan 附带原生 tools 时不加（部分 Provider 不支持二者并用）。
- Perceive 产出 `NeedsTools`，由 Go 解析校验后决定路由；缺失或解析失败时保守走 S_PLAN。

## 决策四：对话历史作为回合上下文进入内核（原 ADR-0098 决策四）

- `protocol.AgentController.SetConversationHistory([]types.Message)`：session 在 `SetTaskIntent` 前注入本轮之前的历史（剔除 system 角色）。
- 历史按 `thresholds.m4_kernel.conversation.history_max_messages` / `history_max_bytes` 取窗；现行实现为各阶段统一写入 L2、只追加、分块跳窗（ADR-0105 决策四，取代原"仅 Perceive/Respond 使用"）。

## 决策五：工具路径真实执行后的既有缺陷收口（原 ADR-0098 决策五）

- **L1 PolicyGate 与执行闸门同问**：`(agent, tool_execute, <tool>)` + 工具 trust_tier 等元数据（词汇单源 `internal/protocol/policy_vocab.go`）；生产装配须 `InjectPolicyGate(sb.Gate)`（此前零调用点，L1 恒 fail-closed 拒绝一切）。
- **L0 孤立节点**：无边独立节点（并行 tool_calls）合法；依赖指向未定义节点才拒绝（`pkg/graph/dag.go`、`internal/execute/dag/validator.go`）。
- **`abortTurn` 是回合唯一中止出口**（`internal/agent/agent_turn.go`）：Dispatch/Effect 错误一律错误进流 + 强制 S_FAILED + `handleTerminalState`（发 task_done）。
- **S_REPLAN 单一 ReplanDone 产出方**：转移占位 Effect 与 `handleReplanTransition` 只能一方投递，否则第二次命中 no transition。
- **召回预算**：内核在调用边界以 3s 预算放弃等待（`recallWithin`），召回是增益不是关键路径。
- **能力令牌 JIT 签发（M07 §6）**：节点通过 S_VALIDATE 后、调用前 JIT Mint（MaxCalls=1、TTL 5min），仅注入该次调用 ctx，返回即撤销。令牌只为本 Agent 最近一次通过 S_VALIDATE 的计划节点签发，工具名与参数须与校验时逐字节一致（`recordValidatedPlan` / `withJITCapability`，`agent_capability.go`；校验开始即作废旧依据，崩溃续跑在重校验通过后重建）；Saga 补偿动作未经 L1 校验，不签发。令牌证明"该调用属于已校验计划"，**不构成对 trust<3 工具的独立审批**：此类工具的信任差异来自安装时用户授权、L1_taint 人工复核（决策十）与 Cedar forbid；进程外 MCP 服务端行为不受本进程令牌约束。
- **流式 tool_calls 收尾补发**：非 `finish_reason=tool_calls` 收尾时已聚合的工具调用不得静默丢弃。

## 决策六：重规划闭环——失败原因回灌规划与回复（原 ADR-0098 决策六）

- 失败原因以结构化短句写入 `StateContext.ReplanFeedback`（最近 3 条、每条 ≤400 字节），来源为 Go 侧校验/执行错误，不含模型自由文本。
- S_PLAN prompt 附 `<previous_attempts_failed>`（TaintMedium 数据区）；`kernel/plan.md` 规定：不得重复被拒方案；允许的工具都无法达成目标则返回空计划 → `S_PLAN_EMPTY` 转 S_RESPOND 如实说明。
- S_RESPOND 同样看到这些原因，回复不得声称已完成被拒绝的操作。
- 安全边界不变：被拒工具仍按 HE-2 拒绝；闭环只改变模型下一次选择，不放宽任何门控。

## 决策七：空输出重试与步数上限（原 ADR-0098 决策七）

- `TriggerFillRetry`：S_PLAN / S_RESPOND 推理成功但既无正文也无工具调用时，按 Effect `MaxRetry`（=1）自环重试（DeepSeek 思考模式挂载工具时偶发只有思考链）。
- 空输出重试改用 `ThinkingDisabled`（`planEffect`，`PlanAttempts>0`）：DeepSeek 省略 `thinking` 字段即默认开启思考，适配器对显式 `ThinkingDisabled` 发送 `thinking.type=disabled`，未指定（空串）保持服务端默认。门控 `TestPlanEffect_RetryDisablesThinking`、`TestDisableDeepSeekThinking`。
- 规划输出有内容但不可用且无缓存 DAG：升一级模型档位经 `TriggerFillRetry` 重试一次（每回合一次，与规划模型自评超纲共用额度），再失败才 S_PLAN_FAILED（ADR-0105 决策二；门控 `TestPlanEffect_RetriesEmptyOutputOnce`）。规划阶段本身失败走 S_FAILED（决策九不变）。
- `m4_kernel.max_steps` = 24（`state.yaml`）：7（完整工具回合）+ 观察循环 2×5（决策八）+ 校验失败 1×3 + 空输出重试 2 + 耗尽转回复 1 = 23 → 24；上界由 ReplanGuard 决定。MAX_STEPS 截断经 `abortTurn` 收尾。

## 决策八：观察—再规划循环（原 ADR-0098 决策八）

- 反思 `GoalAchieved` 显式为 false 时 `S_REFLECT --reflect_continue--> S_REPLAN`（`TriggerReflectContinue`），与校验失败/回滚共用 ReplanGuard，不新增循环上界；仅 `replanCount+1 < MaxReplan` 时继续，否则直接回复（不触发耗尽）。
- 每轮执行结果作为观察累积于 `StateContext.Observations`（最近 4 条、每条 ≤`ObservationMaxBytes` 4KB，首尾保留，ADR-0020 决策五），S_PLAN 据此规划、S_RESPOND 据全部观察作答。
- 反思字段缺失或解析失败按"已达成"处理：宁可少跑一轮。

## 决策九：重规划耗尽转回复（原 ADR-0098 决策九）

- 进入 S_REPLAN 时预算已满 → 转 S_RESPOND（携带失败原因）如实说明，不以 `replan guard: max replan count reached` 技术报错结束。
- `StateContext.TurnDegraded=true`：回合以对话方式结束，但任务结果、漂移分与终态回调按失败计。
- S_FAILED 保留给内核错误（`abortTurn`）、KillSwitch、预算硬上限、感知/规划/回复阶段本身的失败。

## 决策十：回合内人工审批经对话流呈现（原 ADR-0098 决策十）

- 发起：`validateWithTaintReview`（`internal/agent/agent_taint_review.go`）在 L1_taint 节点级拦截时发起 `CheckpointTaintReview`（`ExemptionFieldContent` = 节点参数原始字节），批准后重新校验；每节点每回合至多送审一次，同一工具+参数被拒后本回合不再询问（原因写入 ReplanFeedback）；未装配 HITL 或 ReviewChecker 时不发起。
- 呈现：`types.AgentStreamEventApproval`（Content=checkpoint ID，ToolName/ToolInput/DeadlineNs）→ session 映射 `status{type:"approval_required"}` → Web 就地审批卡片 → `POST /v1/approvals/{id}/resolve`。事件只负责可见性，裁决唯一通道仍是 HITL 网关；四类 Agent 发起的 HITL 统一经 `promptHITLInTurn`。
- 安全边界不变：放行凭证是按参数字节哈希铸造的豁免令牌（HE-2）；`TaintLevel>=Medium` 超时一律拒绝（`resolveTimeoutAction`）；Cedar / 能力令牌 / 执行闸门照常复核。
- `ExemptionVault` 每 Agent 多枚（按内容哈希匹配，上限 16，最旧先淘汰，`internal/security/token/exemption_vault.go`）；TaintMedium write_network 同样查询复核豁免（M11 §3）。

## 决策十一：S_REPLAN 扩展激活重试与降级标记（原 ADR-0065 / A-3）

- `S_REPLAN` 分支异步调 `FindAndActivate` 按需激活扩展；`activateExtWithRetry`（`internal/agent/fsm/state_machine.go`）有限重试，耗尽后 `metrics.GlobalReplanExtActivationDegradedTotal.Add(1)` 并置 `sessionCtx.ReplanExtActivationDegraded`，仍照常 dispatch `TriggerReplanDone`（不阻塞 FSM，HE-5）。
- 消费点：DAG 执行遇 `"tool not found"` 且标记为真 → 快速失败（`internal/agent/agent_execute_dag.go`），不放任空转重试。
- 不新增 FSM 状态（不动 `state.yaml` 状态数）。

## 反例守护 / 已驳方案

| 方案 | 驳回理由 |
|---|---|
| 让 Plan/Reflect 直接把散文回复流给用户、省掉 S_RESPOND | 内部阶段输出契约是 JSON，泄出即本 ADR 所修缺陷复现（决策一/二） |
| `LLMFillEffect` 默认 AudienceUser、内部显式声明 Internal | 违反决策一 fail-closed 默认值 |
| 按 FSM 当前状态判断是否推 token | 受众是 Effect 的属性而非状态的属性，且把输出安全绑在转移表形状上 |
| 流式过滤器剥离 JSON 围栏，保留 Plan 散文作回复 | 概率过滤当边界（HE-2）；Plan 失败时仍无回复 |
| Perceive 与 Respond 合并为一次调用（先答后判路由） | 流式时尚不知是否需要工具，已推文字无法撤回；注：ADR-0105 决策十 4b′ 的 `Reply` 为非流式、经拦截后由确定性 Effect 发布，与此不同 |
| 单一 ReAct 工具循环替代 FSM | 违反 HE-5 与 M4 §1 |
| 降级标记不足时默认新增 FSM 状态 | 先扩大降级标记消费点覆盖面；状态数变更须独立评估 |

## 重新评估触发条件

- Eval 显示纯对话回合 Perceive 路由误判率（应直答却走 Plan，或反之）>10%。
- 主力 Provider 支持"单次调用内结构化路由 + 流式正文"且可在首 token 前确定路由（可重提合并方案）。
- 需要工具的回合中 Respond 调用的 token 占比 >40% 且用户侧无质量差异证据。
- 降级标记消费点覆盖不全致仍有反复重规划空转（决策十一）。

## 沿革

| 日期 | 变更 | 原编号 |
|---|---|---|
| 2026-07-23 | S_REPLAN 扩展激活重试 + 降级标记 | 0065 |
| 2026-09-25 | 初稿：根因为内部填空与用户回复共用 token 通道、FSM 无回复态、记忆路径无输出 Schema、历史未入内核 | 0098 |
| 2026-09-25 | 决策五复核：JIT 令牌绑定已校验计划节点，订正"纵深不减" | 0098 |
| 2026-09-25 | 决策七：空输出重试改关思考（原 `SelectThinkingMode` 恒 Max 使原样重试无意义；`ThinkingDisabled` 在 DeepSeek 上此前从未生效）；有内容但不可解析改升档重试一次 | 0098 追记、0102 决策六 |
| 2026-09-25 | Perceive 增可选 `Reply`（直答合并），S_RESPOND 仍是唯一用户正文出口 | 0102 决策四 4b′ |
| 2026-10-12 | 0065 并入；按代码核实，决策四历史范围改指 ADR-0105 | — |

## 引用代码

- `pkg/types/enums_agent.go`、`pkg/types/models_agent.go`（`AgentStreamEventApproval`）、`internal/protocol/interfaces_agent.go`、`internal/protocol/policy_vocab.go`
- `internal/agent/fsm/transitions_respond.go`、`internal/agent/fsm/state_machine.go`、`internal/agent/fsm/replan_feedback.go`、`internal/agent/fsm/phase_results.go`、`internal/agent/fsm/observations.go`
- `internal/agent/agent_turn.go`、`internal/agent/agent_capability.go`、`internal/agent/agent_taint_review.go`、`internal/agent/agent_execute_dag.go`、`internal/agent/agent_execute_effect_helpers.go`
- `internal/agent/context/respond_context.go`、`internal/gateway/session/orchestrator_fsm.go`、`internal/security/token/exemption_vault.go`
- `configs/prompts/kernel/`、`docs/arch/M04-Agent-Kernel.md §1.1`、`docs/arch/spec/state.yaml`（`m4_kernel.max_steps`）
