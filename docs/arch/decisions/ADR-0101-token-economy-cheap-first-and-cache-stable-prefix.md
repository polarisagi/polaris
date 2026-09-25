# ADR-0101: Token 经济——零 LLM 寒暄快路、规划便宜池先行级联、缓存稳定前缀

- **状态**: Accepted（决策一～三）/ Proposed（决策四）
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

- `SelectThinkingMode(replanCount, complexity, SI)`：重规划或 SI≥high → Max；`complexity ≥ m4_kernel.plan.reasoning_complexity`(0.7) 或 SI≥low → High；否则 Disabled。**污点不再是输入**——污点是来源安全标签，由 Taint/Cedar/PolicyGate 消费；思考深度是成本旋钮，二者正交。
- `SelectPlanModelPool`：ThinkingMax 或高复杂度 → `reasoning`；否则 `general`（经 poolFallbackChain 落到 `default` 即 flash）。
- Perceive 模板为 `Complexity` 补评分标尺，使级联信号可用。
- S_VALIDATE L3 看门狗仍用 `reasoning`（安全补充信号，不在本 ADR 降档范围）。

### 决策三：缓存稳定前缀

- Anthropic：system 按消息边界拆 block，首块（ImmutableCore：人格/工具摘要/偏好，跨会话跨阶段稳定）与末块各一断点，外加最近 2 条消息，共 ≤4。无 system 时退回 tools 末尾断点。
- `SysEnvSnapshot` 只写静态量（OS/CPU/总内存/用户/时区），易变量走 `sys_probe`。
- DeepSeek/OpenAI 为自动前缀缓存，约束即"前缀字节级稳定"：ImmutableCore 已按 stable→volatile 排序、偏好排序输出（`immutable_core_prompt.go`），新增 prompt 段落必须追加在易变区之后。

### 决策四（Proposed，下一阶段）：回合调用数压缩

目标：直答回合 1 次 LLM、单步工具回合 2 次。
1. **Perceive 并入 Plan**：单次"作答或规划"调用挂原生 tools——模型直接给正文即回复（替代 S_PERCEIVE_DIRECT），给 tool_calls 即 DAG；Go FSM 仍持控制流（HE-5），LLM 输出只填槽。
2. **Reflect 并入下一轮 Plan / Respond**：观察—再规划（ADR-0098 决策八）由下一次规划调用携带观察完成，全部节点成功且无继续信号时直接 Respond。
3. 前置门控：`turn_contract_eval_test.go` 增补"每回合 LLM 调用数"断言与真实 Provider 回放 Eval（HE-4），先有基线再动 FSM 转移表。

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
- `internal/observability/metrics/metrics_handler.go`（`SelectThinkingMode` / `SelectPlanModelPool`）
- `internal/agent/fsm/transitions_respond.go`（`planEffect`）
- `internal/llm/adapter/anthropic_request.go`、`internal/sysinfo/sysinfo.go`
- 门控：`TestTurnContractEval_PhaticSkipsPerceive`、`TestPlanEffect_CheapFirstCascade`、`TestClassifyIntentWeight`、`TestBuildAnthropicRequest_StablePrefixBreakpoint`

## 重新评估触发条件

1. `polaris.cognition.turn_route_total{route="phatic_bypass"}` 对应回合的用户负反馈率显著高于 `direct`，或出现寒暄误判吞掉任务的实例 → 收紧词表。
2. 首轮 general 池规划导致的重规划率较基线上升超过 30% → 调低 `plan.reasoning_complexity` 或恢复首轮 ThinkingHigh。
3. Provider 转为"思考深度由模型自适应"且贵/便宜模型价差 < 3× → 重议池级联的必要性。

## 修订记录

- 2026-09-25 创建。
