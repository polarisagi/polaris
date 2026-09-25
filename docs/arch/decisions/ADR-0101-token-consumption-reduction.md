# ADR-0101: Token 消耗治理——关停无主后台回合、按回合蒸馏、分阶段思考档位、稳定缓存前缀

- **状态**: Accepted
- **日期**: 2026-09-25
- **决策者**: 架构组
- **相关模块**: M1（internal/llm）/ M4（internal/agent）/ M5（internal/memory）/ M9（internal/learning）/ M10（internal/knowledge）

## 上下文

2026-09-23~24 两天调试期 DeepSeek 计费：2,203 次请求、14,076,136 token、¥42.60（≈¥3.0/百万 token，均值 6.4K token/请求）。均价远高于缓存命中价，说明输入大部分未命中前缀缓存、且推理/输出 token 占比高。本地库已随重建删除，无法按调用方回溯，故按代码逐条审计请求来源，并以 2026-09-25 重启后的新库取证。

外部约束（2026-09 官方文档）：

- DeepSeek 前缀缓存以"缓存前缀单元"整块匹配，单元在消息边界产生；后续请求须**完整匹配**一个单元才命中（api-docs guides/kv_cache）。命中价约为未命中价的 1/50（V4.1 Flash：$0.003 vs $0.15 /M）。
- DeepSeek 思考模式**省略即开启、effort=high**（guides/thinking_mode）；推理 token 按输出价计费，输出价为未命中输入价的 4 倍。
- 长程 Agent 缓存研究（arXiv 2601.06007）：稳定内容前置、易变内容（时间戳、工具结果、按请求变化的片段）后置，可降低 41–80% API 成本。

审计发现（按量级排序）：

1. **Curriculum 无主后台回合**。`self_improve.auto_curriculum=false` 全仓无人读取；空闲时 M9 中环与 `BackgroundTaskScheduler` 两个入口各每 2 分钟向黑板投递最多 10 个模板任务，`DefaultTaskWorker` 逐个认领并跑完整无头 Agent 回合（Perceive→Plan(max 思考)→Execute→Reflect→Respond + 蒸馏）。取证：重启后 4 分钟内投递并执行 12 个 `ac_*` 任务（新库未配 provider 故 0 token 失败）。
2. **思考档位全面偏高**。Perceive/Reflect（JSON 结构化判定）与全部后台调用未指定档位 → DeepSeek 默认 high；首轮规划因用户输入恒为 TaintHigh，`SelectThinkingMode` 恒返回 max。
3. **记忆蒸馏按反思触发**。每次 S_REFLECT 完成即投递 `memory_consolidate`，管线每次查询整场会话（最多 200 条）重做抽取、摘要、画像合成；观察—再规划回合内反思多次即重复多次。
4. **缓存前缀被易变内容打断**。ImmutableCore 把日期 `VolatileBlock` 与按本轮问题挑选的 `AmbientContext`（技能全文）拼在第一条 system 消息内，问题一变整条系统提示词不命中。
5. **工具定义重复计费**。S_PLAN 把全部工具连同 JSON 参数 schema 写入文本目录，同一请求又经 function-calling 参数下发一遍（内置 56 个工具 schema 约 15KB）。

6. **费用集中在贵档模型**（DeepSeek 控制台：几乎全部费用来自 pro）。S_PLAN（携带全部工具 schema、最高思考档位，单次最贵）与 L3 看门狗固定走 `reasoning` 池；未指定池的调用只按 healthScore 择优（成本权重 0.2，DeepSeek 两档适配器费率同为占位值），在 flash 与 pro 之间近乎随机；`llmInfer` 把池名当模型 ID 传。
7. **无法按调用归因**。流式路径（内核主路径）完全不记用量；非流式只写 `llm.call.recorded` 事件；DeepSeek 的缓存命中（顶层 `prompt_cache_hit_tokens`）与推理 token（`completion_tokens_details.reasoning_tokens`）从未被解析；非流式响应的 `Model` 误填为响应 ID。

## 决策

**关停未授权的后台回合；记忆蒸馏每回合一次；各阶段思考档位显式配置、机械性后台调用关闭思考；系统提示词稳定层与易变层分两条消息；工具参数 schema 只经 function-calling 下发一次；每次调用落库记账；日常调用一律走便宜档模型。**

- **决策一**：`self_improve.auto_curriculum` 门控 Curriculum 两个入口（M9 引擎传 nil 生成器；`BackgroundTaskScheduler` 生成器为 nil 时只跑红队探测）。
- **决策二**：`memory_consolidate` 从 S_REFLECT 完成移到回合终态（`emitConsolidateOnce`），每回合恰好一次，回放期间不投递。
- **决策三**：新增 `ThinkingLow`（DeepSeek `reasoning_effort=low`）与 `m4_kernel.thinking.{perceive,plan_initial,reflect,respond}`（默认 low / high / low / ""，加载时校验）；首轮规划取配置档位，重规划仍按 `SelectThinkingMode` 升到 max，空输出重试仍关闭思考。机械性后台调用显式 `ThinkingDisabled`：会话摘要、压缩摘要、记忆写入过滤、持续记忆、GraphRAG 抽取/社区摘要/概念构建、RAG 摘要树、Reflexion、画像精炼、`llmInfer`（同时修正其把池名当模型 ID 传的缺陷）。事实性判官、查询改写、合成评测、Prompt 优化器、任务分解、LAM、技能/插件创建保持原档位。
- **决策四**：`ImmutableCore.PrependToMessages` 输出两条 system 消息：稳定层（身份、指令、工具名、偏好）与易变层（`# VOLATILE CONTEXT` 日期 + AmbientContext）。内容不变，只拆分边界。
- **决策五**：S_PLAN 文本工具目录只列名称与描述，参数 schema 仅经 function-calling 下发。
- **决策六**：新表 `llm_calls`（`039_llm_calls.sql`）每次 Provider 调用一行：时间、会话（`CtxTaskIDKey`）、用途（新增 `types.WithPurpose`，内核按 FSM 阶段、后台按调用点标注，未标注记 `unspecified`）、Provider 注册名、模型 ID、模型池、思考档位、流式与否、状态、输入/缓存命中/输出/推理 token、耗时、按 `ProviderCapabilities` 费率估算的费用、错误。写入点是注册表对每个 Provider 的记录包装，覆盖路由主路径、failover、降级池、溢出升级与 `PickProvider` 直取；有界异步队列（512，满即丢弃并告警），记账不阻塞推理。`ProviderRegistry.Get` 返回原始实例，保留 `protocol.LocalProvider` 等类型断言。OpenAI 兼容 usage 统一经 `OpenAIUsage.toUsage` 解析，兼容 DeepSeek 顶层缓存字段与推理 token；非流式响应 `Model` 取响应的 `model` 字段。
- **决策七**：内核各阶段模型池可配置 `m4_kernel.model_pool.{perceive,plan_initial,plan_replan,reflect,respond,validate}`，默认除 `plan_replan=reasoning` 外全部为 `default`（便宜档）；只有首轮计划被拒或未达成目标后的重规划才用贵档，设为 `default` 即完全不用贵档。未指定池的请求按角色成本档位（default/budget/未标角色 → general → reasoning）由低到高择优，同档再比 healthScore。`llmInfer` 改走 `default` 池。DeepSeek 适配器按模型填官方费率（flash $0.30/$1.20/$0.006、pro $1.32/$3.96/$0.044 每百万，高峰价），flash 上下文更正为 1M。

## 后果

- **正向**: 日常对话与规划的费用从贵档移到便宜档；`llm_calls` 可直接回答"哪个用途、哪个模型花了多少"；空闲期不再产生 token；多轮回合的蒸馏调用从"每轮反思一次"降到"每回合一次"；结构化阶段与后台调用的推理 token 大幅下降；系统提示词稳定层跨问题、跨阶段字节不变，可命中缓存；每次规划少付一份工具 schema。
- **负向**: Perceive/Reflect 降为 low、首轮规划由 max 降为 high 可能影响路由与规划质量——`turn_contract_eval_test` 全绿，但它不调用真实模型，**须以真实 Key 跑一次回合契约评测确认**（HE-4），不达标按 `state.yaml` 调回档位即可，无需改代码。不支持 function-calling 的 Provider 在 S_PLAN 失去文本参数 schema。
- **反例守护**: 新增后台 LLM 调用不显式指定思考档位（DeepSeek 省略即 high）；把按请求变化的内容拼入第一条 system 消息；在回合内的中间阶段触发全会话蒸馏；新增后台自动任务不经配置开关——引用本 ADR 拒绝。

## 未在本 ADR 落地（第二批，需评测或更大改造）

| 项 | 现状 | 方向 |
|------|------|------|
| 会话历史可缓存化 | 历史以单条 `<conversation_history>` 消息注入，窗口按条/字节逐轮滑动，每轮都不同，永不命中缓存 | 按真实消息逐条注入、分块跳窗（满阈值一次裁掉一半而非逐条滑动），配合压缩摘要，使历史前缀只追加 |
| 其余后台调用点的用途标注 | 事实性判官、查询改写、合成评测、Prompt 优化器、任务分解、LAM、技能/插件创建等仍记为 `unspecified` | 按 `llm_calls` 中 `unspecified` 的实际占比补标 |
| llm_calls 保留期 | 无清理 | 按 `created_at` 纳入事件归档/清理周期 |
| 蒸馏抽取截断 | 抽取文本取事件拼接的前 8000 字符；会话一长，每次处理的都是同一段开头 | 按水位只处理上次蒸馏后的新增事件 |
| 语义抽取事件类型错配 | Agent 投递 `task_perceived` 等类型，`PerMessageExtractor` 只认 `observation/tool_call/reflection`，准实时抽取从未生效 | 功能缺陷，单独修复（修复后注意它会新增 LLM 调用，须同时关闭思考并做去重） |
| 召回记忆位置 | 召回结果位于历史与意图之前，随查询变化，打断其后的全部前缀 | 稳定内容（历史）前置、召回与本轮意图后置 |

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 在适配器层把"未指定档位"统一改为 disabled | 会同时改变 OpenAI 兼容透传接口与面向用户回复的行为；显式按调用点声明可审计、可逐项回滚 |
| 默认禁用多 Agent | 多 Agent（`spawn_planner`、`transfer_to_agent`、debate）已由模型经工具调用自行决定，常驻 swarm Agent 只做数据库扫描不调 LLM；PRM 多候选默认关闭。无需新增门控 |
| 回合内所有阶段共用一次 LLM 调用（合并 Perceive/Plan） | 与 ADR-0098 的阶段契约与 FSM 控制流冲突（HE-5）；阶段拆分的前缀代价由决策四降低 |

## 引用代码

- `cmd/polaris/boot_agent.go`、`internal/learning/curriculum/curriculum_scheduler.go`（决策一）
- `internal/agent/agent_lifecycle.go` `emitConsolidateOnce`、`internal/agent/agent_execute_effect_helpers.go`（决策二）
- `pkg/types/enums_llm.go` `ThinkingLow`/`ParseThinkingMode`、`internal/config/thresholds.go` `M4KernelThresholds.Validate`、`internal/agent/fsm/transitions.go`、`transitions_respond.go` `phaseThinking`、`internal/llm/adapter/client.go`（决策三）
- `internal/memory/store/immutable_core_prompt.go` `PrependToMessages`/`volatileSystemContent`（决策四）
- `internal/agent/context/tool_list_section.go`（决策五）
- `internal/protocol/schema/039_llm_calls.sql`、`internal/llm/usage_recorder.go`、`internal/store/repo/repo_llm_call.go`、`internal/llm/adapter/client.go` `toUsage`（决策六）
- `internal/agent/fsm/transitions_respond.go` `planModelPool`、`internal/llm/provider_registry.go` `bestWith`/`costTier`、`internal/llm/adapter/deepseek.go`（决策七）
- `docs/arch/spec/state.yaml §thresholds.m4_kernel.thinking_* / model_pool_*`

## 重新评估触发条件

- 真实 Key 回合契约评测中 Perceive 路由（NeedsTools）准确率或 Reflect 判定准确率较调整前下降：对应阶段档位调回 high。
- 规划 schema 失败率 >5% 或重规划率明显上升：`thinking.plan_initial` 调回 max。
- 接入不支持 function-calling 的 Provider 作为规划模型：为其恢复文本参数 schema。
- `llm_calls` 显示便宜档首轮规划的重规划率较贵档时期明显上升、总费用反升：`model_pool.plan_initial` 调回 reasoning。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-25 | 初稿 |
