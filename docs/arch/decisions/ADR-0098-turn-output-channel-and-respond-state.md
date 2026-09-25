# ADR-0098: Agent 回合输出通道分离与 S_RESPOND 回复合成态

- **状态**: Accepted
- **日期**: 2026-09-25
- **决策者**: 架构组
- **相关模块**: M4 / M13（gateway/session）/ internal/protocol / pkg/types

## 上下文

2026-09-24 实测：桌面/Web 对话「你好。你是谁？」返回内容为 S_PLAN 阶段模型原始输出（散文 + ```json 围栏 DAG + 散文），回合实际以 `S_PLAN → S_FAILED` 结束。四个叠加根因（均有日志/代码实证）：

1. `doStreamInfer` 把**所有** LLMFillEffect（Perceive/Plan/Reflect）的 token 一律以 `AgentStreamEventToken` 推给订阅者——内部结构化填空与用户回复共用一条通道。
2. FSM 13 态中**不存在**产出用户回复的阶段；此前用户能看到文字，仅因 Plan 阶段模型"不守规矩"写了散文并被第 1 点泄出。
3. 有记忆系统（生产路径）时 Perceive/Plan/Reflect prompt 由 `agent/context` 内联一句话生成，不加载 `configs/prompts/kernel/*.md`，**无输出 Schema**；模型自编格式，解析必然失败。Perceive 输出还从未被解析进 TaskModel（`onPerceiveSuccess` 只判非空）。
4. session 只把本轮 `req.Input` 交给内核（`SetTaskIntent`），多轮对话历史从未进入 FSM；Pool 每轮终态后新建 Agent，内核对上文完全失忆。

## 决策

**内部阶段与用户回复物理分通道；新增 S_RESPOND 作为回合唯一的用户回复出口；Perceive 产出结构化路由字段，由 Go FSM 决定走直答还是规划执行。**

### 决策一：输出受众（Audience）由 Effect 声明，零值 fail-closed

- `protocol.LLMFillEffect.Audience`：`AudienceInternal`（零值）/ `AudienceUser`。只有 `AudienceUser` 的 Effect 其 token 才以 `AgentStreamEventToken` 发布；内部 Effect 的 token 只累积进 `resp.Content` 供 OnSuccess 解析。
- 零值即 Internal：未来新增阶段忘记声明时默认**不外泄**（安全默认值优于 opt-out）。
- 推理思考链（`AgentStreamEventThinking`）不受 Audience 约束，各阶段照常发布——它在前端独立渲染为"思考过程"，不进回复正文、不落消息历史。
- 阶段进度以结构化 `AgentStreamEventPhase`（Content=阶段键 `perceive|plan|execute|reflect|respond`）发布，客户端自行本地化展示；禁止用自然语言状态串承载阶段语义（R1.6）。

### 决策二：S_RESPOND 为第 14 态，是 Complete 的唯一前驱

| From | Trigger | To | 条件 |
|---|---|---|---|
| S_PERCEIVE | TriggerRespondReady | S_RESPOND | TaskModel.NeedsTools == false（直答） |
| S_PLAN | TriggerRespondReady | S_RESPOND | 解析成功且 DAG 为空（含 FastPath 无缓存 DAG） |
| S_REFLECT | TriggerReflectDone | S_RESPOND | 原 → S_COMPLETE 改指向 |
| S_RESPOND | TriggerRespondDone | S_COMPLETE | 回复非空 |
| S_RESPOND | TriggerRespondReady | S_RESPOND | 空回复且未超 MaxRetry（自环重试，未推出 token） |
| S_RESPOND | TriggerReplanExhausted | S_FAILED | 推理失败 / 重试后仍空回复（A-01），原因以错误事件发布 |

- 枚举值**追加**在尾部（`AgentStateRespond` / `TriggerRespondReady` / `TriggerRespondDone`）：状态以 `%d` 落盘于 EventLog（崩溃恢复白名单按值匹配），插入中间会使历史事件错位。
- S_RESPOND 是纯 LLM 协处理器态、无外部副作用，列入崩溃恢复重驱白名单。
- 回复 prompt 输入：ImmutableCore（人格 + 用户偏好）+ `kernel/respond.md` + 有界对话历史 + 本轮用户消息 +（若执行过）任务目标、执行结果、反思结论。执行结果与反思按 TaintMedium 起、对话历史按 TaintHigh 进 UserData 围栏。

### 决策三：内部阶段输出契约单源化 + 约束解码

- `configs/prompts/kernel/{perceive,plan,reflect,respond}.md` 是阶段契约唯一来源，记忆路径与降级路径都加载它（此前记忆路径绕开模板）。
- Perceive / Reflect 请求 `json_object` 约束解码；Plan 在附带原生 tools 时不加（部分 Provider 不支持二者并用），靠 tool_calls + 围栏剥离收敛。
- Perceive 产出 `NeedsTools`：由 Go 解析、校验后决定路由；缺失或解析失败时**保守走 S_PLAN**（完整管线仍可在 Plan 得出空 DAG 后转直答），不因感知失败放弃回合。

### 决策四：对话历史作为回合上下文进入内核

- `protocol.AgentController.SetConversationHistory([]types.Message)`：session 在 `SetTaskIntent` 前注入本轮之前的历史（剔除 system 角色）。
- 仅 Perceive（解析指代、产出自包含 Goal）与 Respond（连贯作答）使用；Plan 只消费自包含的 TaskModel.Goal，不重复携带历史（Tier-0 token 纪律）。
- 历史按 `thresholds.m4_kernel.conversation.history_max_messages` / `history_max_bytes` 自尾部截取。

### 决策五：S_RESPOND 使工具路径首次被真实执行，路径上的既有缺陷一并收口

Plan 阶段此前恒解析失败，Validate/Execute 从未被生产流量走到；修复后暴露并收口：

- **L1 PolicyGate 与执行闸门同问**：`(agent, tool_execute, <tool>)` + 工具 trust_tier 等元数据（`protocol/policy_vocab.go` 为词汇单源）；生产装配补 `InjectPolicyGate(sb.Gate)`（此前零调用点，L1 恒拿 nil 按 fail-closed 拒绝一切）。
- **L0 孤立节点 → 悬空依赖**：无边独立节点（并行 tool_calls）合法；依赖指向未定义节点才拒绝。
- **回合唯一中止出口 `abortTurn`**：Dispatch/Effect 错误一律错误进流 + 强制 S_FAILED + `handleTerminalState`（发 task_done）；此前订阅方等不到 task_done 挂到超时，且 Pool 会把下一轮投给已无 Run() 的旧实例。
- **S_REPLAN 单一 ReplanDone 产出方**：进入 S_REPLAN 的转移占位 Effect 与 handleReplanTransition 各投递一次 → 第二次命中 no transition。
- **召回预算**：search.Embedder 无 ctx（下游固定 30s），内核在调用边界以 3s 预算放弃等待（`recallWithin` / `assembleWithBudget`），召回是增益不是关键路径。
- **能力令牌 JIT 签发接线（M07 §6）**：通用工具路径此前从不签发令牌——写类工具必被执行闸门 Step 3 拒绝，trust<3（MCP/社区）工具因 `tool_execute_permit` 要求令牌而全部不可执行。现：节点通过 S_VALIDATE 后、调用前 JIT Mint（MaxCalls=1、TTL 5min），仅注入该次调用 ctx，返回即撤销（执行闸门只 Verify 不 Consume，撤销保证一次性）。L1 预检中 `capability_token_valid` 表达"通过本闸门即签发"（=true），执行闸门以真实令牌复核，纵深不减。
  - > 2026-09-25 复核（订正"纵深不减"）：初稿按工具名无条件签发，而 L1 又以 `capability_token_valid=true` 评估，执行闸门对 trust<3 工具的令牌要求实为恒真，原句不成立。现令牌只为**本 Agent 最近一次通过 S_VALIDATE 的计划节点**签发——工具名与参数须与校验时逐字节一致（`agent_capability.go` `recordValidatedPlan` / `withJITCapability`；校验开始即作废旧依据，崩溃续跑在重校验通过后重建），Saga 补偿动作未经 L1 策略校验，不签发。令牌由此证明"该调用属于已校验计划"，**不构成对 trust<3 工具的独立审批**：此类工具的信任差异来自安装时的用户授权、L1_taint 人工复核（决策十）与 Cedar forbid；进程外 MCP 服务端的行为本不受本进程令牌约束，另加令牌审批只会是形式防线。
- **流式 tool_calls 收尾补发**：非 `finish_reason=tool_calls` 收尾时已聚合的工具调用不再被静默丢弃。

### 决策六：重规划闭环——失败原因回灌规划与回复

S_VALIDATE 拒绝或 S_EXECUTE 失败后进入 S_REPLAN，此前重规划 prompt 与首次完全相同，模型不知道上一版为何被拒，反复产出同一计划直到 ReplanGuard 耗尽（2026-09-25 实测：`bash` 因 TaintHigh 参数被 L1_taint 拒绝，三次重规划均再选 `bash`）。

- 失败原因以结构化短句写入 `StateContext.ReplanFeedback`（有界：最近 3 条、每条 ≤400 字节），来源为 Go 侧校验/执行错误，不含模型自由文本。
- S_PLAN prompt 附 `<previous_attempts_failed>`（TaintMedium 数据区）；`kernel/plan.md` 规定：不得重复被拒方案，换用允许的工具；若允许的工具都无法达成目标则返回空计划——FSM 经 `S_PLAN_EMPTY` 转 S_RESPOND，由回复阶段如实说明限制。
- S_RESPOND 同样看到这些原因，回复不得声称已完成被拒绝的操作。
- 安全边界不变：被拒工具仍按 HE-2 拒绝；闭环只改变模型的下一次选择，不放宽任何门控。

### 决策七：空输出重试与步数上限

- 新增 `TriggerFillRetry`（枚举尾部追加）：S_PLAN / S_RESPOND 推理成功但**既无正文也无工具调用**时，按 Effect `MaxRetry`（=1）自环重试。实证：DeepSeek 思考模式挂载工具时偶发 `finish_reason=stop`、只有思考链（2026-09-25 `llm stream: ended without content or tool calls ... completion_tokens=316`）。S_RESPOND 原借用 `respond_ready` 的自环改为 `fill_retry`。
- `m4_kernel.max_steps` 10 → 24：步数按回合内 FSM 触发计。S_RESPOND 使完整工具回合为 7 步，上限 10 使第 2 次重规划即被 MAX_STEPS 截断，ReplanGuard（3 次）形同虚设；上界应由 ReplanGuard 决定：7 + 观察循环 2×5（决策八）+ 校验失败 1×3 + 空输出重试 2 + 耗尽转回复 1 = 23 → 24。
- MAX_STEPS 截断经 `abortTurn` 收尾（此前 ForceState 后直接返回，订阅方等不到 task_done 挂到超时，2026-09-25 实测）。

> 2026-09-25 追记（空输出重试关闭思考）：上线后空 Plan 仍偶发"重试后依然为空 → S_PLAN_FAILED"。新事实两条：(1) 用户输入恒 TaintHigh，`SelectThinkingMode` 对首轮规划恒返回 ThinkingMax，原样重试是在同一触发条件下再试一次；(2) DeepSeek 省略 `thinking` 字段即默认开启思考（effort=high，api-docs.deepseek.com guides/thinking_mode），`translateRequest` 的"不发送即关闭"对它不成立——`ThinkingDisabled` 在 DeepSeek 上从未生效。处置：S_PLAN 空输出重试改用 `ThinkingDisabled`（`planEffect`，`PlanAttempts>0`）；DeepSeek 适配器对显式 `ThinkingDisabled` 发送 `thinking.type=disabled`，未指定（空串）保持服务端默认。副作用：经 Router 且未指定思考档的调用（`protocol.ApplyInferOptions` 默认 `ThinkingDisabled`）在 DeepSeek 上变为真正不思考，这是该枚举的设计语义，此前的"默认 high"是适配器缺陷。门控：`TestPlanEffect_RetryDisablesThinking`、`TestDisableDeepSeekThinking`（注入原缺陷均报红）。决策九"规划阶段本身的失败走 S_FAILED"不变。

### 决策八：观察—再规划循环

一轮执行的结果不足以达成目标时（反思 `GoalAchieved` **显式为 false**），回到规划阶段继续，而不是带着不完整结果硬写回复（2026-09-25 实测：回复阶段模型因信息不足输出 `<tool_calls>` 标记试图继续调用工具）。

- `S_REFLECT --reflect_continue--> S_REPLAN`（`TriggerReflectContinue`，枚举尾部追加），与校验失败 / 回滚共用同一 ReplanGuard，不新增循环上界；仅在 `replanCount+1 < MaxReplan` 时继续，否则直接回复（不触发耗尽）。
- 每轮执行结果作为**观察**累积于 `StateContext.Observations`（最近 4 条、每条 ≤4KB），S_PLAN 据此规划下一步而非重复已做的事，S_RESPOND 据全部观察作答。
- 反思字段缺失或解析失败按"已达成"处理：宁可少跑一轮，不因输出不规范空转。

### 决策九：重规划耗尽转回复

进入 S_REPLAN 时预算已满，改转 S_RESPOND（携带失败原因），由回复阶段如实说明，而非以 `replan guard: max replan count reached` 技术报错结束（2026-09-25 实测：模型连续选择被拒的 `bash` 直至耗尽，用户只收到报错）。

- `StateContext.TurnDegraded=true`：回合以对话方式结束，但任务结果、漂移分与终态回调按**失败**计，指标不被"有回复"美化。
- S_FAILED 保留给内核错误（`abortTurn`）、KillSwitch、预算硬上限、感知/规划/回复阶段本身的失败。

### 决策十：回合内人工审批经对话流呈现

S_VALIDATE L1_taint 拦截（用户输入恒 TaintHigh，由其派生参数的非只读工具必然被拦，ADR-0007）的既定转义路径是 SanitizeByUserReview（M11 §2.5），但此前 S_VALIDATE 从不发起复核，回合只能"解释为什么不能做"；Agent 发起的其余 HITL（盲区、出口污点、设备操控）只在自动化页轮询可见，对话里的用户看不到，回合挂到超时。

- **发起**：`validateWithTaintReview`（agent_taint_review.go）在 L1_taint 节点级拦截时发起 `CheckpointTaintReview`，`ExemptionFieldContent` = 节点参数原始字节；批准后重新校验。每节点每回合至多送审一次（批准无效不空转），同一工具+参数被拒后本回合不再询问（拒绝原因写入 ReplanFeedback，模型改道或经 S_PLAN_EMPTY 转回复说明）。未装配 HITL 或 ReviewChecker 时不发起（批准无从生效）。
- **呈现**：`types.AgentStreamEventApproval`（Content=checkpoint ID，ToolName/ToolInput/DeadlineNs）→ session 映射 `status{type:"approval_required"}` → Web 对话就地审批卡片 → `POST /v1/approvals/{id}/resolve`。事件只负责可见性，裁决唯一通道仍是 HITL 网关；四类 Agent 发起的 HITL 统一经 `promptHITLInTurn`。
- **安全边界不变**：放行凭证是按参数字节哈希铸造的豁免令牌（HE-2），参数变一个字节即不匹配；`TaintLevel>=Medium` 超时一律拒绝（`resolveTimeoutAction`），信任评分降级不适用；Cedar / 能力令牌 / 执行闸门照常复核。
- **ExemptionVault 每 Agent 多枚**（按内容哈希匹配，上限 16，最旧先淘汰）：原"每 Agent 一枚覆盖写"使多节点逐个批准时后者冲掉前者，审批永不收敛。
- **TaintMedium write_network 同样查询复核豁免**：M11 §3 规定其转义路径为 SanitizeByUserReview，此前只有 TaintHigh 查询，Medium 拦截即使批准也无从放行。

## 后果

- **正向**: 用户永远只看到 S_RESPOND 的回复；内部 JSON 不再外泄（交互 SSE 与 headless 的 `AgentResult.Output` 同时修复）；纯对话从"Perceive+Plan 且必失败"变为 Perceive+Respond 两次调用；多轮对话恢复上下文连贯。
- **负向**: 需执行工具的回合多一次 LLM 调用（Respond）；首 token 延迟增加一个 Perceive 往返，以阶段进度事件缓解。
- **反例守护**:
  - 提议"让 Plan/Reflect 直接把散文回复流给用户、省掉 S_RESPOND"——违反决策一/二：内部阶段输出契约是 JSON，泄出即本 ADR 所修缺陷的原样复现。
  - 提议"LLMFillEffect 默认 AudienceUser、内部阶段显式声明 Internal"——违反决策一的 fail-closed 默认值。
  - 提议"按 FSM 当前状态判断是否推 token"——受众是 Effect 的属性而非状态的属性（同一状态未来可能挂多个 Effect），且状态判断把输出安全绑在转移表形状上。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 流式过滤器剥离 JSON 围栏，保留 Plan 散文作为回复 | 概率过滤当边界（HE-2）；模型换措辞即失效；Plan 失败时仍无回复 |
| Perceive 与 Respond 合并为一次调用（先答后判路由） | 流式输出时尚不知是否需要工具，已推出的文字无法撤回；回到"内部输出外泄"原问题 |
| 单一 ReAct 工具循环替代 FSM | 违反 HE-5（`while True: call LLM`）与 M4 §1 |

## 引用代码

- `pkg/types/enums_agent.go`（AgentStateRespond / TriggerRespondReady / TriggerRespondDone）
- `internal/protocol/interfaces_agent.go`（LLMFillEffect.Audience / AgentController.SetConversationHistory）
- `internal/agent/fsm/transitions_respond.go`（S_RESPOND 转移与 respond Effect）
- `internal/agent/agent_execute_effect_helpers.go`（doStreamInfer 按 Audience 发布）
- `internal/agent/context/respond_context.go`（BuildRespondContext）
- `internal/gateway/session/orchestrator_fsm.go`（注入历史、Phase 事件映射）
- `docs/arch/M04-Agent-Kernel.md §1.1`、`docs/arch/spec/state.yaml §par`

## 重新评估触发条件

- Eval Harness 显示纯对话回合 Perceive 路由误判率（应直答却走 Plan，或反之）> 10%。
- 主力 Provider 支持"单次调用内结构化路由 + 流式正文"且可在首 token 前确定路由（可重提合并方案）。
- 需要工具的回合中 Respond 调用的 token 占比 > 40% 且用户侧无质量差异证据。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-25 | 初稿 |
| 2026-09-25 | 决策五复核：JIT 令牌绑定已校验计划节点（工具名+参数），订正"纵深不减" |

> 2026-09-25 追记（ADR-0101 决策四 4b′）：Perceive 契约新增可选 `Reply`，`NeedsTools=false` 时同次产出回复；Perceive→Respond 入边据此以确定性 Effect 进入 S_RESPOND 并由 Agent 以用户受众发布。决策一"只有 S_RESPOND 向用户发布正文"不变——Perceive 的流式 token 仍为内部受众，不逐 token 推送；发布前经 `publishableReply` 拦截内部产物特征。
