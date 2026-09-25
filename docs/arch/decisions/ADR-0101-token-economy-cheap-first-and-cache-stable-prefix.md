# ADR-0101: Token 经济——零 LLM 寒暄快路、规划便宜池先行级联、缓存稳定前缀

- **状态**: Accepted（决策一～六；决策二的触发规则被决策六取代；决策四原 4b/4c 草案被 4b′ 取代）
- **日期**: 2026-09-25
- **决策者**: 架构组
- **相关模块**: M01 / M04 / M05 / `internal/agent/fsm` / `internal/llm/adapter` / `internal/sysinfo`

## 上下文

代码事实（2026-09-25 审计）：

1. 每回合固定链路 Perceive(LLM) → Plan(LLM) → Validate → Execute → Reflect(LLM) → Respond(LLM)。"你好"/"谢谢"也要 Perceive + Respond 两次 LLM，且 Perceive 伴随一轮 episodic/反思/语义/RAG 召回（含 embedding 调用）。
2. `SelectThinkingMode(replan, maxTaint, SI)` 以 `TaintLevel ≥ 3` 为 ThinkingMax 条件；用户输入恒为 TaintHigh（ADR-0098 决策七追记已记录此事实），`planEffect` 又硬编码 `ModelPool: "reasoning"`——**每个**首轮规划都走 Pro + 满档思考，`[inference] default_provider = deepseek-flash` 形同虚设。`spec/state.yaml` 写 perceive/plan 为 `budget`，代码与规范性文档已漂移。
3. Anthropic 适配器把全部 system 消息拼成一段、只在末尾打一个断点：阶段指令/核心记忆/工具目录任一变化即令最前面的人格前缀整段失配。
4. `SysEnvSnapshot` 含"已用内存/磁盘剩余"，每会话取值不同，位于 prompt 最前（无记忆降级路径）。

外部依据（2026 年检索）：
- Manus《Context Engineering for AI Agents》：KV-cache 命中率是生产 Agent 的首要指标；前缀须字节级稳定、上下文只追加、序列化确定性。
- DeepSeek 自动磁盘前缀缓存：V4 Flash 命中价约为未命中的 1/50（$0.003 vs $0.15 /MTok，off-peak），无需显式标记，只要求前缀稳定。
- 级联路由（RouteLLM 及 2026 后续 IRT-Router / "Cluster, Route, Escalate" arXiv 2606.27457、"Is Escalation Worth It?" arXiv 2605.06350）：便宜模型先行、按置信度/失败信号升级，典型在 ≥95% 质量下省 50%~85% 成本。
- "Not All Turns Are Equally Hard"（arXiv 2604.05164）与 overthinking 研究：简单轮次满档思考既费 token 又可能降质。
- JetBrains "The Complexity Trap"（arXiv 2508.21433）：观察遮蔽与 LLM 摘要同效且便宜 ~50%——本仓 ADR-0100 的确定性修剪 + ToolRefOffloader 已对齐，不再追加 LLM 摘要。
- "At Equal Inference Cost, Multi-Agent Structure Does Not Beat a Single Frozen Agent"（arXiv 2609.04217）及多份 2026 评测：同预算下单 Agent 不弱于 planner/executor 多 Agent，后者 token 4~220×。

## 决策

### 决策一：零 LLM 寒暄快路（System-0）+ 短确认精简感知

`fsm.ClassifyIntentWeight` 以纯字符串规则（≤16 rune、剥标点/emoji/语气词、整句精确匹配）把输入分三级：
- `IntentPhatic`（问候/致谢/告别）：S_IDLE→S_PERCEIVE 的 Effect 直接是 `DeterministicEffect`，写入 `TaskModel{NeedsTools:false}` 并返回 `S_PERCEIVE_DIRECT`——跳过 Perceive LLM 与全部召回，只调一次不挂工具的 Respond。
- `IntentAck`（好的/ok/同意）：**不得**直答（可能是对上一轮提议动作的授权），仍走 Perceive，但跳过长期记忆召回，只保留画像与对话历史。
- `IntentFull`：原路径。

判定只收窄成本路径，不放宽安全路径：Phatic 只能到达不挂工具的 S_RESPOND。宁漏判不误判。

### 决策二：规划便宜池先行、失败再升级（修订 ADR-0020 决策二）

> 2026-09-25 修订：下列"重规划即 Max"的触发规则被**决策六**取代（按失败成因升级、`SelectPlanTier` 单一阶梯）；"污点不参与""便宜池先行"原则不变。原文保留作历史。

- `SelectThinkingMode(replanCount, complexity, SI)`：重规划或 SI≥high → Max；`complexity ≥ m4_kernel.plan.reasoning_complexity`(0.7) 或 SI≥low → High；否则 Disabled。**污点不再是输入**——污点是来源安全标签，由 Taint/Cedar/PolicyGate 消费；思考深度是成本旋钮，二者正交。
- `SelectPlanModelPool`：ThinkingMax 或高复杂度 → `reasoning`；否则 `general`（经 poolFallbackChain 落到 `default` 即 flash）。
- Perceive 模板为 `Complexity` 补评分标尺，使级联信号可用。
- S_VALIDATE L3 看门狗仍用 `reasoning`（安全补充信号，不在本 ADR 降档范围）。

### 决策三：缓存稳定前缀

- Anthropic：system 按消息边界拆 block，首块（ImmutableCore：人格/工具摘要/偏好，跨会话跨阶段稳定）与末块各一断点，外加最近 2 条消息，共 ≤4。无 system 时退回 tools 末尾断点。
- `SysEnvSnapshot` 只写静态量（OS/CPU/总内存/用户/时区），易变量走 `sys_probe`。
- DeepSeek/OpenAI 为自动前缀缓存，约束即"前缀字节级稳定"：ImmutableCore 已按 stable→volatile 排序、偏好排序输出（`immutable_core_prompt.go`），新增 prompt 段落必须追加在易变区之后。

### 决策四：回合调用数压缩

目标：直答回合 1 次 LLM、单步工具回合 2 次。

**4a（Accepted，2026-09-25）简单任务成功跳过 Reflect LLM**：`trySkipReflect` 在"首轮（replanCount=0）+ `ExecAllSucceeded` + 0 < Complexity < `m4_kernel.reflect.skip_complexity`(0.4)"时以 `DeterministicEffect` 直接 `S_REFLECT_DONE`（route=`reflect_skipped`），并清空上一轮 `Reflection`。`ExecAllSucceeded` 由 `runExecuteDAG` 每次执行重置、全部节点 `Success` 且未降级重规划才置真。复杂度缺失（0）/偏高、存在失败、重规划轮次一律保留 LLM 反思。简单工具回合 4 次 LLM → 3 次。

**4b′（Accepted，2026-09-25）直答合并进 Perceive**：Perceive 契约新增可选字段 `Reply`——`NeedsTools=false` 时模型在同一次调用里写出最终回复。`applyPerceiveResult` 经 `publishableReply`（拦截 `"Goal"`/`"NeedsTools"`/`"nodes"`/tool-call 标记等内部产物特征）后存入 `StateContext.PreparedReply`；**只有** Perceive→Respond 入边（`directRespondEffects`）消费它，以 `DeterministicEffect` 进入 S_RESPOND，Agent 在该 Effect 中以用户受众发布正文（`publishPreparedReply`）。S_RESPOND 仍是唯一向用户发布正文的状态（par_inv_06 不变）。回合起点（S_IDLE→S_PERCEIVE）清空 `PreparedReply`，System-1/FastPath/寒暄旁路不会发布陈旧回复。`Reply` 为空或被拦截 → 照常 Respond LLM。直答回合 2 次 LLM → 1 次（route=`direct_merged`）。
代价：直答不再逐 token 流式（整段一次发布）；回复由 Perceive（general 池、json_object）写出，与原 Respond 同池同思考档，质量口径不变。

**原 4b/4c 草案（驳回）**：
- "Perceive 并入 Plan（单次调用挂 tools，给正文即回复）"：Plan 是内部受众（ADR-0098 决策一），直答正文仍需 Respond 或流式泄漏风险回归；且去掉 Perceive 即失去 Complexity，决策二级联与 4a 失去判据。4b′ 以更小改动拿到直答 1 次调用。
- "Reflect 并入下一轮 Plan"：单步工具回合反而多一次 Plan 调用（4 次不变）；简单任务已由 4a 跳过，复杂任务的反思（观察—再规划、learnings）保留价值。
- 门控：`turn_contract_eval_test.go` 已对三条路由断言每回合 LLM 调用数（寒暄 1 次 Respond、直答合并 0 次 Respond、简单工具回合 0 次 Reflect）。

### 决策五：S_PLAN 工具定义去重

S_PLAN 已经原生 function-calling 下发完整工具定义（`WithTools`），`BuildToolListSection` 此前又把"名称 + 描述 + 参数 JSON Schema"全文写进 prompt——最大的一块上下文每次规划计费两次（按每工具 ~200 token 计，30 个工具 ≈ 6K token/次）。改为文本目录只列名称（JSON-DAG 输出路径只需 action 合法取值）。无原生 tools 的 `LocalAdapter`（llama.cpp，`SupportsTools=false`）在适配器内把 `WithTools` 定义渲染为文本插在前导 system 之后（`withToolsAsText`），能力差异止于适配器层。附带收益：MCP 工具描述不再出现在文本目录，间接注入面收窄（S-02）。

### 决策六：LLM 判难度、程序持策略；按失败成因升级（取代决策二的触发规则）

新事实：决策二仍以 `replanCount > 0` 触发 reasoning + ThinkingMax。但重规划有五种成因，只有"模型能力不足"一类能靠换更强模型解决——安全闸门拒绝换 Pro 照样被拒；超时/限流换 Pro 照样超时；反思判定未达成是观察—再规划的正常推进。

**分工**：
- **LLM 给语义信号**（它能看懂任务，程序不能）：Perceive（便宜模型）输出 `Complexity`，模板给出锚定标尺并要求"两档间取低档——失败会自动升级"（便宜优先 + 兜底升级，对应 AutoMix/DiSRouter 一类自评路由；小模型自评校准有限，故只作起点，不作唯一依据）。规划模型可输出 `{"escalate": true}` 自评超纲（plan.md 规则 9）。
- **程序持有策略**（确定性、可审计、可单测）：`metrics.SelectPlanTier(escalation, complexity, SI)` 一张阶梯表决定池与思考档；`escalation` 只由程序观测到的**能力类失败**累计。

**阶梯**：0 general/无思考 → 1 general/High → 2 reasoning/High → 3 reasoning/Max。起点：Complexity ≥ 0.7 → 2；SI ≥ high → +1；SI ≥ low → 至少 1。

**失败成因 → 升级**（`fsm.RecordFailure`）：

| 成因 | 来源 | 升级 |
|---|---|---|
| `FailurePolicy` | S_VALIDATE L1_taint / L1_policy / L2_heuristic / L3_llm | 否 |
| `FailureTransient` | 执行超时、网络、限流、取消、Provider 耗尽、TOCTOU 冲突 | 否 |
| `FailureGoalUnmet` | 反思未达成 → 观察—再规划 | 否 |
| `FailureToolError` | 工具报错（参数错、对象不存在） | 首次否，第二次起 +1 |
| `FailurePlanInvalid` | S_VALIDATE L0 结构错误；规划输出有内容但不可解析 | +1 |
| `FailureSelfEscalate` | 规划模型输出 `escalate=true` | 直接到 2 |

**规划输出不可用的升级重试**：此前"有内容但解析失败"且无缓存 DAG 时直接 S_PLAN_FAILED 结束回合；现改为升一级后经 `TriggerFillRetry` 重试一次（与自评超纲共用每回合一次的额度 `PlanEscalated`）。空输出仍按 ADR-0098 决策七关闭思考重试，不计入升级（Provider 行为问题，非能力问题）。

效果：重规划中最常见的安全拒绝与瞬时故障不再付 Pro + Max；真正的能力失败得到逐级而非一步到顶的升级（Flash 开思考往往已足够）。

## 后果

- **正向**：寒暄回合 LLM 调用 2→1、召回 1→0；简单工具任务规划从 Pro+Max 降到 Flash+无思考（按 V4 定价，规划输入约 1/4、输出与思考 token 大幅下降）；Anthropic 人格前缀在阶段间共享缓存。
- **负向**：首轮便宜池规划的失败率可能上升，代价是一次重规划（届时升级到 Pro+Max）；Complexity 由 LLM 自评，存在低估风险。
- **反例守护**：
  - 拒绝"用户输入 TaintHigh → 满档思考/贵模型"——污点与推理深度正交，防御由五防线承担。
  - 拒绝把 `IntentAck` 纳入直答旁路——"好的"可能授权破坏性动作。
  - 拒绝在 prompt 前部加入秒级时间戳、实时资源用量、无序 map 渲染。
  - 拒绝为省 token 引入 LLM 摘要式上下文压缩作为首选（见 Complexity Trap）；先确定性修剪/卸载。
  - 拒绝默认启用多 Agent 拆分常规任务；多 Agent 仅用于可并行的独立子任务或需要隔离的专家（M08）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 用小模型/embedding 分类器判定寒暄 | 为省一次 LLM 调用再加一次模型调用，且需要评测集；规则表零成本、可审计 |
| 规划恒用 budget 池（state.yaml 旧值） | 无 budget 角色 Provider 时 poolFallbackChain 为空，直接落到不限 role 的健康度择优，可能选中 Pro，结果不确定 |
| 所有阶段都走 reasoning 池保质量 | 与 `default_provider = flash` 的配置意图相悖，成本放大一个数量级 |

## 引用代码

- `internal/agent/fsm/intent_gate.go`、`internal/agent/fsm/transitions.go`（`tryPhaticBypass`）
- `internal/agent/context/memory_context.go`（`BuildPerceiveContext` 短确认精简召回）
- `internal/observability/metrics/metrics_handler.go`（`SelectPlanTier`；决策二的 `SelectThinkingMode`/`SelectPlanModelPool` 已由其取代删除）
- `internal/agent/fsm/escalation.go`（`FailureKind`/`RecordFailure`）、`internal/agent/agent_turn.go`（`validationFailureKind`/`executionFailureKind`）、`internal/agent/fsm/transitions_respond.go`（`tryEscalatePlan`/`planSelfEscalates`）
- `internal/agent/fsm/transitions_respond.go`（`planEffect`）
- `internal/llm/adapter/anthropic_request.go`、`internal/sysinfo/sysinfo.go`
- `internal/agent/fsm/transitions.go`（`trySkipReflect`）、`internal/agent/agent_execute_dag.go`（`ExecAllSucceeded`）
- `internal/agent/context/tool_list_section.go`、`internal/llm/adapter/local.go`（`withToolsAsText`）
- `internal/agent/fsm/phase_results.go`（`publishableReply`）、`internal/agent/fsm/transitions_respond.go`（`directRespondEffects`）、`internal/agent/agent_turn.go`（`publishPreparedReply`）、`configs/prompts/kernel/perceive.md`（`Reply`）
- 门控（决策六）：`TestPlanEffect_CheapFirstCascade`、`TestPlanEffect_RetriesEmptyOutputOnce`、`TestFailureKindClassification`、`TestTurnContractEval_RejectionFeedbackLoop`（安全拒绝后规划池不升级）
- 门控：`TestTurnContractEval_DirectReplyMergedIntoPerceive`、`TestPerceiveDirectReplyMerge`、`TestTurnStartClearsPreparedReply`、`TestTurnContractEval_SimpleToolTaskSkipsReflect`、`TestTrySkipReflect`、`TestBuildToolListSection_NamesOnly`、`TestWithToolsAsText`、`TestTurnContractEval_PhaticSkipsPerceive`、`TestPlanEffect_CheapFirstCascade`、`TestClassifyIntentWeight`、`TestBuildAnthropicRequest_StablePrefixBreakpoint`

## 重新评估触发条件

1. `polaris.cognition.turn_route_total{route="phatic_bypass"}` 对应回合的用户负反馈率显著高于 `direct`，或出现寒暄误判吞掉任务的实例 → 收紧词表。
2. 首轮 general 池规划导致的重规划率较基线上升超过 30% → 调低 `plan.reasoning_complexity` 或恢复首轮 ThinkingHigh。
3. `reflect_skipped` 回合的用户纠正/重问率显著高于完整反思回合 → 调低或关闭 `reflect.skip_complexity`。
4. `direct_merged` 回合出现内部产物泄漏或用户对直答质量的负反馈显著高于 Respond LLM 路径 → 关闭合并（perceive.md 去掉 `Reply`）。
5. `plan_escalated` 占规划调用比例 > 20%，或 level 0 规划的 S_VALIDATE L0 失败率显著高于 level ≥ 2 → 便宜模型规划能力不足，调低 `plan.reasoning_complexity` 或将起点提到 level 1。
6. Provider 转为"思考深度由模型自适应"且贵/便宜模型价差 < 3× → 重议池级联的必要性。

## 修订记录

- 2026-09-25 创建。
- 2026-09-25 决策四 4a 落地（简单任务跳过反思）；新增决策五（工具定义去重）。
- 2026-09-25 新增决策六（按失败成因升级、SelectPlanTier 阶梯），取代决策二触发规则。
- 2026-09-25 决策四 4b′ 落地（直答合并进 Perceive）；原 4b/4c 草案驳回并记录理由。
