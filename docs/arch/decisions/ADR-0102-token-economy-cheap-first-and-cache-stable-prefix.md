# ADR-0102: Token 经济增量——零 LLM 寒暄快路、直答合并、按失败成因升级、反思跳过

- **状态**: Accepted（决策一、三～六；决策二并入决策六）
- **日期**: 2026-09-25
- **决策者**: 架构组
- **相关模块**: M01 / M04 / M05 / `internal/agent/fsm` / `internal/llm/adapter` / `internal/sysinfo`
- **前置**: ADR-0101（Token 消耗治理：阶段思考档位与模型池配置、ImmutableCore 稳定层/易变层、`llm_calls` 记账）。本 ADR 是其上的增量，不重复其决策。

## 上下文

ADR-0101 之后仍存在的代码事实（2026-09-25 审计）：

1. 每回合固定链路 Perceive(LLM) → Plan(LLM) → Validate → Execute → Reflect(LLM) → Respond(LLM)。"你好"/"谢谢"也要 Perceive + Respond 两次 LLM，且 Perceive 伴随一轮 episodic/反思/语义/RAG 召回（含 embedding 调用）；普通直答固定 2 次；简单工具任务固定 4 次。
2. 重规划仍由 `SelectThinkingMode` 一律升到 max 并切 `model_pool.plan_replan`（贵档）。但重规划有五种成因，只有"模型能力不足"一类能靠换更强模型解决——安全闸门拒绝换 Pro 照样被拒；超时/限流换 Pro 照样超时；反思判定未达成是观察—再规划的正常推进。规划输出有内容但不可解析时则直接 S_PLAN_FAILED，既不重试也不升级。
3. Anthropic 适配器把全部 system 消息拼成一段、只在末尾打一个断点：ADR-0101 决策四拆出的稳定层在 Anthropic 上得不到独立缓存。
4. `SysEnvSnapshot` 含"已用内存/磁盘剩余"，每会话取值不同，位于无记忆降级路径 prompt 最前。
5. ADR-0101 决策五保留了工具描述文本，原生 tools 已携带同一描述；不支持 function-calling 的 Provider 失去参数 schema（ADR-0101 自述的负向后果）。

外部依据（2026 年检索）：
- Manus《Context Engineering for AI Agents》：KV-cache 命中率是生产 Agent 的首要指标；前缀须字节级稳定、上下文只追加。
- 级联/自评路由：RouteLLM 及 2026 后续（IRT-Router、"Cluster, Route, Escalate" arXiv 2606.27457、"Is Escalation Worth It?" arXiv 2605.06350）；AutoMix、DiSRouter（arXiv 2510.19208）——小模型自评可区分难易但校准有限，适合作起点、以客观失败信号兜底升级。
- "Not All Turns Are Equally Hard"（arXiv 2604.05164）：简单轮次满档思考既费 token 又可能降质。
- AgentErrorTaxonomy / "Where LLM Agents Fail"（arXiv 2509.25370）：失败按成因分类处置，重试预算后再交更强模型。
- JetBrains "The Complexity Trap"（arXiv 2508.21433）：观察遮蔽与 LLM 摘要同效且便宜 ~50%——ADR-0100 的确定性修剪 + ToolRefOffloader 已对齐，不追加 LLM 摘要。
- "At Equal Inference Cost, Multi-Agent Structure Does Not Beat a Single Frozen Agent"（arXiv 2609.04217）：同预算下多 Agent 不占优，token 4~220×（与 ADR-0101 "多 Agent 由模型经工具自行决定"一致，不新增门控）。

## 决策

### 决策一：零 LLM 寒暄快路（System-0）+ 短确认精简感知

`fsm.ClassifyIntentWeight` 以纯字符串规则（≤16 rune、剥标点/emoji/语气词、整句精确匹配）把输入分三级：
- `IntentPhatic`（问候/致谢/告别）：S_IDLE→S_PERCEIVE 的 Effect 直接是 `DeterministicEffect`，写入 `TaskModel{NeedsTools:false}` 并返回 `S_PERCEIVE_DIRECT`（route=`phatic_bypass`）——跳过 Perceive LLM 与全部召回，只调一次不挂工具的 Respond。
- `IntentAck`（好的/ok/同意）：**不得**直答（可能是对上一轮提议动作的授权），仍走 Perceive，但跳过长期记忆召回，只保留画像与对话历史。
- `IntentFull`：原路径。

判定只收窄成本路径，不放宽安全路径：Phatic 只能到达不挂工具的 S_RESPOND。宁漏判不误判。

### 决策二：并入决策六

（初稿为"污点不参与思考档位、复杂度驱动规划池"；ADR-0101 决策三/七已把首轮规划改为配置档位与便宜池，余下的升级规则统一由决策六表述。编号保留以免代码注释引用错位。）

### 决策三：缓存稳定前缀（补 ADR-0101 决策四）

- Anthropic：system 按消息边界拆 block，首块（ADR-0101 决策四的稳定层：人格/指令/工具名/偏好，跨会话跨阶段稳定）与末块各一断点，外加最近 2 条消息，共 ≤4。无 system 时退回 tools 末尾断点。
- `SysEnvSnapshot` 只写静态量（OS/CPU/总内存/用户/时区），易变量走 `sys_probe`。

### 决策四：回合调用数压缩

**4a 简单任务成功跳过 Reflect LLM**：`trySkipReflect` 在"首轮（replanCount=0）+ `ExecAllSucceeded` + 0 < Complexity < `m4_kernel.reflect.skip_complexity`(0.4)"时以 `DeterministicEffect` 直接 `S_REFLECT_DONE`（route=`reflect_skipped`），并清空上一轮 `Reflection`。`ExecAllSucceeded` 由 `runExecuteDAG` 每次执行重置、全部节点 `Success` 且未降级重规划才置真。复杂度缺失（0）/偏高、存在失败、重规划轮次一律保留 LLM 反思。简单工具回合 4 次 LLM → 3 次。

**4b′ 直答合并进 Perceive**：Perceive 契约新增可选字段 `Reply`——`NeedsTools=false` 时模型在同一次调用里写出最终回复。`applyPerceiveResult` 经 `publishableReply`（拦截 `"Goal"`/`"NeedsTools"`/`"nodes"`/tool-call 标记等内部产物特征）后存入 `StateContext.PreparedReply`；**只有** Perceive→Respond 入边（`directRespondEffects`）消费它，以 `DeterministicEffect` 进入 S_RESPOND，Agent 在该 Effect 中以用户受众发布正文（`publishPreparedReply`）。S_RESPOND 仍是唯一向用户发布正文的状态（par_inv_06 不变）。回合起点清空 `PreparedReply`。`Reply` 为空或被拦截 → 照常 Respond LLM。直答回合 2 次 LLM → 1 次（route=`direct_merged`）。
代价：直答不再逐 token 流式（整段一次发布）；回复由 Perceive 写出（`model_pool.perceive`、`thinking.perceive`），若其档位低于 Respond，直答质量随之——两者都由 ADR-0101 阈值控制，可按评测调整。

驳回的替代草案：
- "Perceive 并入 Plan（单次调用挂 tools，给正文即回复）"：Plan 是内部受众（ADR-0098 决策一），直答正文仍需 Respond 或流式泄漏风险回归；去掉 Perceive 即失去 Complexity，决策六与 4a 失去判据（同 ADR-0101 被驳回方案第三行）。
- "Reflect 并入下一轮 Plan"：单步工具回合反而多一次 Plan 调用（4 次不变）；简单任务已由 4a 跳过，复杂任务的反思保留价值。

### 决策五：S_PLAN 工具目录只列名称（收紧 ADR-0101 决策五）

原生 tools 已携带名称、描述与参数 schema；文本目录只保留名称（JSON-DAG 输出路径只需 action 合法取值）。无原生 tools 的 `LocalAdapter`（llama.cpp，`SupportsTools=false`）在适配器内把 `WithTools` 定义渲染为文本插在前导 system 之后（`withToolsAsText`），解除 ADR-0101 自述的"不支持 function-calling 的 Provider 失去参数 schema"负向后果；能力差异止于适配器层。附带收益：MCP 工具描述不再出现在文本目录，间接注入面收窄（S-02）。新增不支持原生 tools 的适配器须同样渲染 `InferOptions.Tools`。

### 决策六：LLM 判难度、程序持策略；按失败成因升级（取代 `SelectThinkingMode` 的"重规划即 Max"）

**分工**：
- **LLM 给语义信号**（它能看懂任务，程序不能）：Perceive（便宜模型）输出 `Complexity`，模板给出锚定标尺并要求"两档间取低档——失败会自动升级"。规划模型可输出 `{"escalate": true}` 自评超纲（plan.md 规则 9）。
- **程序持有策略**（确定性、可审计、可单测）：`metrics.SelectPlanTier(escalation, complexity, SI)` 一张阶梯表决定池与思考档；`escalation` 只由程序观测到的**能力类失败**累计。

**阶梯**（两端取 ADR-0101 阶段配置）：

| level | 模型池 | 思考档 |
|---|---|---|
| 0 | `model_pool.plan_initial`（默认 default） | `thinking.plan_initial`（默认 high） |
| 1 | 同上 | 上调一档（≤low→high，high→max） |
| 2 | `model_pool.plan_replan`（默认 reasoning） | high |
| 3 | 同上 | max |

起点：Complexity ≥ `m4_kernel.plan.reasoning_complexity`(0.7) → 2；SI ≥ high → 加 1；SI ≥ low → 至少 1。

**失败成因 → 升级**（`fsm.RecordFailure`）：

| 成因 | 来源 | 升级 |
|---|---|---|
| `FailurePolicy` | S_VALIDATE L1_taint / L1_policy / L2_heuristic / L3_llm | 否 |
| `FailureTransient` | 执行超时、网络、限流、取消、Provider 耗尽、TOCTOU 冲突 | 否 |
| `FailureGoalUnmet` | 反思未达成 → 观察—再规划 | 否 |
| `FailureToolError` | 工具报错（参数错、对象不存在） | 首次否，第二次起 +1 |
| `FailurePlanInvalid` | S_VALIDATE L0 结构错误；规划输出有内容但不可解析 | +1 |
| `FailureSelfEscalate` | 规划模型输出 `escalate=true` | 直接到 2 |

**规划输出不可用的升级重试**：有内容但不可解析且无缓存 DAG 时，升一级后经 `TriggerFillRetry` 重试一次（route=`plan_escalated`，与自评超纲共用每回合一次的额度 `PlanEscalated`），再失败才 S_PLAN_FAILED。空输出仍按 ADR-0098 决策七关闭思考重试，不计入升级。污点不参与（用户输入恒 TaintHigh；安全由五防线承担）。

## 后果

- **正向**：寒暄 2→1 次 LLM、召回 1→0；普通直答 2→1；简单工具任务 4→3、工具定义只付一份；重规划中最常见的安全拒绝与瞬时故障不再付 Pro + Max；能力失败逐级升级而非一步到顶；规划输出偶发不可用不再直接丢回合。
- **负向**：直答非流式；Complexity 由便宜模型自评，存在低估风险（以失败升级兜底）；每回合最多多一次升级重试调用。
- **反例守护**：
  - 拒绝"用户输入 TaintHigh → 满档思考/贵模型"；拒绝"任何重规划都升 Pro + Max"——按成因处置。
  - 拒绝把 `IntentAck` 纳入直答旁路——"好的"可能授权破坏性动作。
  - 拒绝让 Perceive→Respond 以外的入边消费 `PreparedReply`。
  - 拒绝在 prompt 前部加入秒级时间戳、实时资源用量、无序 map 渲染。
  - 拒绝以 LLM 摘要式上下文压缩作为首选（见 Complexity Trap）；先确定性修剪/卸载。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 用小模型/embedding 分类器判定寒暄 | 为省一次 LLM 调用再加一次模型调用，且需评测集；规则表零成本、可审计 |
| 完全交给 LLM 决定用哪个模型（程序照做） | 小模型自评校准有限且可被输入内容诱导，花费的决定权不应交给不可信输出；LLM 只提供信号，阶梯表与额度由程序持有 |
| 完全由程序判定难度（关键词/长度/工具数启发式） | 语义难度程序看不懂；Perceive 本就要调用，附带 Complexity 零额外成本 |
| 规划恒用 budget 池 | 无 budget 角色 Provider 时降级链为空，落到不限 role 的择优，结果不确定 |

## 引用代码

- `internal/agent/fsm/intent_gate.go`、`internal/agent/fsm/transitions.go`（`tryPhaticBypass`、`trySkipReflect`）、`internal/agent/context/memory_context.go`（短确认精简召回）
- `internal/observability/metrics/metrics_handler.go`（`SelectPlanTier`）、`internal/agent/fsm/escalation.go`（`FailureKind`/`RecordFailure`）、`internal/agent/agent_turn.go`（`validationFailureKind`/`executionFailureKind`/`publishPreparedReply`）、`internal/agent/fsm/transitions_respond.go`（`planEffect`/`tryEscalatePlan`/`planSelfEscalates`/`directRespondEffects`）
- `internal/agent/fsm/phase_results.go`（`publishableReply`）、`internal/agent/agent_execute_dag.go`（`ExecAllSucceeded`）
- `internal/llm/adapter/anthropic_request.go`、`internal/sysinfo/sysinfo.go`、`internal/agent/context/tool_list_section.go`、`internal/llm/adapter/local.go`（`withToolsAsText`）
- `configs/prompts/kernel/perceive.md`（`Complexity` 标尺、`Reply`）、`configs/prompts/kernel/plan.md`（规则 9 `escalate`）
- 门控：`TestTurnContractEval_PhaticSkipsPerceive`、`TestTurnContractEval_DirectReplyMergedIntoPerceive`、`TestTurnContractEval_SimpleToolTaskSkipsReflect`、`TestTurnContractEval_RejectionFeedbackLoop`（安全拒绝后规划池不升级）、`TestPlanEffect_CheapFirstCascade`、`TestPlanEffect_RetriesEmptyOutputOnce`、`TestFailureKindClassification`、`TestTrySkipReflect`、`TestPerceiveDirectReplyMerge`、`TestTurnStartClearsPreparedReply`、`TestClassifyIntentWeight`、`TestBuildToolListSection_NamesOnly`、`TestWithToolsAsText`、`TestBuildAnthropicRequest_StablePrefixBreakpoint`

## 重新评估触发条件

1. `turn_route_total{route="phatic_bypass"}` 回合的负反馈率显著高于 `direct`，或出现寒暄误判吞掉任务 → 收紧词表。
2. `direct_merged` 回合出现内部产物泄漏，或直答质量负反馈显著高于 Respond LLM 路径 → 关闭合并（perceive.md 去掉 `Reply`）。
3. `reflect_skipped` 回合的用户纠正/重问率显著高于完整反思回合 → 调低或关闭 `reflect.skip_complexity`。
4. `plan_escalated` 占规划调用 > 20%，或 level 0 规划的 L0 失败率显著高于 level ≥ 2（`llm_calls` 按 purpose/model 归因）→ 调低 `plan.reasoning_complexity` 或提高 `thinking.plan_initial`。
5. Provider 转为"思考深度由模型自适应"且贵/便宜模型价差 < 3× → 重议阶梯的必要性。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-25 | 初稿（分支上原编号 ADR-0101，与 main 同日合入的 ADR-0101 冲突，按编号不复用规则改为 0102，并改写为其增量） |
