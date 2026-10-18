# ADR-0105: Token 经济：后台门控、分档思考与模型池、五层前缀账本、召回单一管线、调用门控与用量账本

> 状态：Accepted（2026-10-12 按代码核实：除「重排门」有接口无加载路径外全部已落地） ｜ 首次：2026-09-25 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0101、ADR-0102、ADR-0105
> 相关模块：M1（`internal/llm`）/ M4（`internal/agent`）/ M5（`internal/memory`）/ M9 / M10。目标：同一回合少调用、调用走便宜档、前缀字节稳定吃缓存、每笔调用可归因。

## 旧引用映射（代码注释里的「ADR-0101/0102/0105 决策X」按此跳转；各节标题亦含原编号，可直接 grep）

| 旧引用 | 新节 | 旧引用 | 新节 |
|---|---|---|---|
| 0101 决策一/二 | 决策一 | 0102 决策六（含决策二） | 决策二 |
| 0101 决策三/七 | 决策二 | 0102 决策一/四 | 决策十 |
| 0101 决策四、0102 决策三、0105 决策一/九 | 决策三 | 0102 决策五、0105 决策三 | 决策六 |
| 0105 决策二 | 决策四 | 0105 决策五/六 | 决策七 |
| 0105 决策四/十 | 决策五 | 0105 决策七 | 决策八 |
| 0105 决策十一 | 决策十一 | 0101 决策六、0105 决策八 | 决策九 |

## 决策一：后台回合开关门控与每回合一次蒸馏（原 ADR-0101 决策一、原 ADR-0101 决策二）

- `self_improve.auto_curriculum` 门控 Curriculum 两个入口（M9 中环传 nil 生成器；`BackgroundTaskScheduler` 生成器为 nil 时只跑红队探测）。新增后台自动任务必须有配置开关。
- `memory_consolidate` 只在回合终态投递一次（`emitConsolidateOnce`），回放期间不投递；禁止在回合中间阶段触发全会话蒸馏。
- 多 Agent 不加门控：`spawn_planner`/`transfer_to_agent`/debate 由模型经工具自决，常驻 swarm Agent 不调 LLM，PRM 多候选默认关。
- **2026-10-18 复核（ADR-0115）**：后台 LLM 任务在 `full` 档默认开启（用于 sleep-time 记忆整理与自改进闭环，WP6）；`standard` 与 `hardened` 档仍受配置开关与错峰门控。

## 决策二：思考档位、模型池与按失败成因升级（原 ADR-0101 决策三、原 ADR-0101 决策七、原 ADR-0102 决策六、原 ADR-0102 决策二）

- 机械类后台调用显式 `ThinkingDisabled`（会话/压缩摘要、记忆写入过滤、持续记忆、GraphRAG 抽取/社区/概念、RAG 摘要树、Reflexion、画像精炼、`llmInfer`）；DeepSeek 省略档位即 high，禁止依赖默认。`ThinkingLow` = `reasoning_effort=low`。
- 阶段档位与模型池可配（阈值 SSoT：`docs/arch/spec/state.yaml §thresholds.m4_kernel` `thinking_*` / `model_pool_*`）：档位 perceive=low / plan_initial=high / reflect=low / respond=""；池除 `plan_replan=reasoning` 外全为 `default`（便宜档）。空输出重试仍关思考。未指定池的请求按角色成本档（default/budget/未标 → general → reasoning）择优，同档比 healthScore；`llmInfer` 走 `default` 池。
- 分工：LLM 只给语义信号（Perceive 输出 `Complexity`，规划模型可输出 `escalate=true`）；策略由程序持有：`metrics.SelectPlanTier(escalation, complexity, SI)` 阶梯，`escalation` 只被**能力类失败**累加。污点不参与选档（用户输入恒 TaintHigh，安全由五防线承担）。

| level | 池 | 思考档 |
|---|---|---|
| 0 | `plan_initial` | `thinking.plan_initial`（high） |
| 1 | 同上 | 上调一档（≤low→high，high→max） |
| 2 | `plan_replan` | high |
| 3 | 同上 | max |

起点：Complexity ≥ `plan.reasoning_complexity`(0.7) → 2；SI ≥ high → +1；SI ≥ low → 至少 1。

| 失败成因（`fsm.RecordFailure`） | 升级 |
|---|---|
| Policy（S_VALIDATE L1/L2/L3 拒绝）、Transient（超时/网络/限流/取消/Provider 耗尽/TOCTOU）、GoalUnmet（反思未达成） | 否 |
| ToolError | 首次否，第二次起 +1 |
| PlanInvalid（L0 结构错；输出有内容但不可解析） | +1 |
| SelfEscalate | 直接到 2 |

- 规划输出有内容但不可解析且无缓存 DAG：升一级后 `TriggerFillRetry` 重试一次（route=`plan_escalated`，与自评超纲共用每回合一次额度），再失败才 S_PLAN_FAILED；空输出按 ADR-0098 关思考重试，不计升级。
- 反例：「TaintHigh → 满档/贵模型」「任何重规划升 Pro+Max」「规划恒用 budget 池（无该角色时降级链为空）」「由程序启发式判难度」「把花费决定权交给 LLM 输出」均已驳回。
- **2026-10-18 复核（ADR-0115）**：推翻「LLM 只给信号、选档归程序」。模型自选模型与思考档；程序阶梯只在模型未表态时生效；成本上限改为用户可选配置，默认不限。
- 重新评估：真实 Key 回合契约评测中 Perceive 路由/Reflect 判定准确率下降 → 对应阶段调回 high；规划 schema 失败率 >5% → `plan_initial` 调回 max；便宜档首轮规划重规划率上升致总费用反升 → `model_pool.plan_initial` 调回 reasoning；`plan_escalated` >20% → 调 `reasoning_complexity`。

## 决策三：五层前缀账本与阶段契约入稳定核（原 ADR-0101 决策四、原 ADR-0102 决策三、原 ADR-0105 决策一、原 ADR-0105 决策九）

所有内核阶段与降级路径消息统一按变化频率排序，同回合各阶段与相邻回合共享最长字节一致前缀：

| 层 | 内容 | 角色 |
|---|---|---|
| L0 稳定核 | 阶段契约库（独立首条 system）→ 身份/指令/平台/运营（配置期）→ 工具名/扩展（安装期）→ 画像与偏好（按键排序） | system |
| L1 会话层 | 核心记忆、子 Agent 画像、工作区上下文、扩展目录 | system |
| L2 历史层 | 逐条真实消息，只追加（决策四） | user/assistant，TaintHigh 逐条围栏 |
| L3 阶段层 | 阶段选择器（`# ACTIVE PHASE: X`）+ 易变层（日期、AmbientContext、压力提示、预算、ToolHints、MCP 连接状态） | system |
| L4 回合层 | 召回、TaskModel、GroundingGap、观测、重规划反馈、执行结果、本轮意图 | user |

- 字节稳定规则（违反即缺陷）：L0/L1 集合渲染必须确定序（排序或 SQL `ORDER BY`；`InMemoryToolRegistry.List()` 按名排序）；L0/L1 禁止时间戳、连接状态、资源用量；`SysEnvSnapshot` 只写静态量，易变量走 `sys_probe`；Spotlighting 标记按单条消息哈希。
- 阶段契约（perceive/plan/reflect/respond.md）渲染为 L0 的 `# PHASE CONTRACTS`（固定顺序，约 7.9KB，不计入 `maxSystemPromptBytes`，超长只截可变部分）；L3 只留一条选择器。开关 `m4_kernel.prompt.phase_contracts_in_core`（默认 true），关闭回到 L3 全文模板。无 ImmutableCore 的降级路径仍在 L3 写完整契约。契约段内禁止任何按会话/阶段变化的文本。`respond_reminder.md` 留在末尾。
- 污点分区语义不变，只改输出次序；`PromptBuilder.BuildLayered` 供内核，旧 `Build` 供非内核调用方。
- Anthropic：断点 L0 末、L2 末、最后一条消息（≤4）；L0 多于一条时首条与末条都标；无 system 退回 tools 末尾；`inline_nonleading_system`（默认 true）把开头连续 system 之后的 system 转 user 角色 `<system_instruction>` 块，适配器无条件转义该标签字面。阶段间思考参数不同使 messages 缓存失效，作为已知代价，不统一档位。
- 反例：map 直接拼接 / 无 ORDER BY；按请求变化的内容放历史之前；历史重新合并成单条滑窗；为命中缓存放宽污点围栏（缓存让位 HE-2/HE-7）。
- 重新评估：阶段模板后移致 Perceive 路由或 Plan schema 失败率上升 >2pp → 阶段层前移到 L1 后、L2 前。

## 决策四：对话历史只追加与分块跳窗（原 ADR-0105 决策二）

- 历史逐条写入 L2（`fsm.WriteConversationHistory`）；累计超 `m4_kernel.conversation.history_max_messages`(20)/`history_max_bytes`(24576) 时一次裁掉前半，裁掉部分由确定性压缩摘要（`agent_context_compaction.go`）以单条锚定消息置于 L2 首位，摘要只在跳窗时重算。
- 回合内四阶段看到相同 L2（Plan/Reflect 亦带历史）。若 `llm_calls` 显示 Plan/Reflect 未命中 token 与总费用上升 → 回退为不带历史。
- 压缩/溢出恢复只动缓存前缀（最后一个 `CacheBreakpoint` 及其前连续 system）之后的内容，L3 选择器原位保留。

## 决策五：召回预算与单一管线（原 ADR-0105 决策四、原 ADR-0105 决策十）

- 全内核只有一处召回、一处注入点（L4，历史之后、本轮意图之前）；旧 `injectMemoryToMsgs`/`Assembler` 旁路已删除（它把按 Goal 变化的内容插进 L1/L2 之间）。Plan 复用本回合 Perceive 的召回；仅 goal 查询词不同且召回为空时补查一次；查询 embedding 回合内缓存。
- 渲染：情景取摘要字段（否则截断 payload），单条 ≤`recall.item_max_chars`(400)，时间戳到日；总量 ≤`recall.max_tokens`(1200)；与 L2 历史重复（规范化后包含）丢弃；跨来源同正文去重。
- 融合：各来源按原生分排序后 RRF(k=60) 加权融合（`recall.weight_{reflection=1.2,episodic=1.1,semantic=1.0,rag=1.0}`，先验值待 Eval 标定），按融合秩装入预算；`min_score_ratio` 默认 0（关闭，保留开关）。画像不入融合（与 L0 `UserProfile` 同源，已删召回画像段）。
- 来源：情景走 FTS BM25（`FTSEpisodic`，项目隔离，再按 ID 取正文，已删子串匹配）；L2 语义实体走 `sement_` FTS（`recallCognitiveAdapter`）；RAG 分透传（`AugmentedContext.Score`），`recall.rag_min_surprise`(0.3) 以下不查 RAG、高于 `surprise_hint_threshold` 时 topK 加倍；MaxTaint 过滤 fail-closed（`None` 按 `High`）。降级（无 SurrealDB/Tier0、FTS 失败）按「该来源无结果」。每类 FTS 文档只经一条路径进 prompt。
- 重排门：消费端接口 `fsm.RecallReranker`（需返回已校准概率；`rerank_top_n`=12、`rerank_min_prob`=0.5，失败/非法一律跳过门）已落地，**生产无实现注入，门不生效**（`ffi.LlamaRerank` 是 bi-encoder 余弦，非 cross-encoder）。接入 logit 型 cross-encoder 须在适配器内先 sigmoid。
- 反例：重提 DTOC `manage_context`、热路径 LLM prompt 压缩（LLMLingua 类）、内核回复语义缓存——均已驳回（见决策七/沿革）。

## 决策六：工具定义下发与 Provider 缓存接线（原 ADR-0101 决策五、原 ADR-0102 决策五、原 ADR-0105 决策三）

- S_PLAN 文本工具目录只列名称；参数 schema 与描述仅经原生 tools 下发。`SupportsTools=false` 的 `LocalAdapter` 在适配器内用 `withToolsAsText` 渲染 `WithTools`；新增不支持原生 tools 的适配器须同样渲染。
- tools 数组只追加：核心工具按名排序在前，`search_tools` 固定其后，本会话激活的工具按激活顺序追加（`catalog/composite.go`）。
- OpenAI 兼容：按能力位发 `prompt_cache_key`（`hash(agent_id)`，无 PII）与可配 `openai.prompt_cache_retention`（默认不传）；Anthropic `anthropic.cache_ttl`（`5m`|`1h`，默认 5m）；DeepSeek 自动前缀缓存；Google 写 `cachedContentTokenCount` 到 `Usage.CacheHitTokens`。
- 实验开关 `m4_kernel.cache.uniform_tools`（默认 false）：Perceive/Reflect/Respond 也下发 Plan 的 tools 并设 `tool_choice=none`；是否默认开启须以 `llm_calls` 实测决定（HE-4）。

## 决策七：调用参数门控与确定性响应缓存（原 ADR-0105 决策五、原 ADR-0105 决策六）

- 每个 `safecall.Infer`/`Provider.Infer`/`InferStream` 调用（`internal/llm` 内部转发除外）必须显式传 `types.WithPurpose` 与 `types.WithThinkingMode`；门控 `tools/llm_call_opts_lint.go`（L-19，AST，变参展开须同函数可见含两项，不可判定即红，已登记 `tools/lint-selftest.txt`）。`Purpose*` 常量集中于 `pkg/types/purposes.go`。
- 默认档位：机械类 Disabled；判官/分类类（factuality、sampling scorer、shadow、查询改写）Low（须 Eval 确认，HE-4）；创作/规划类保留 high。判官 `MaxTokens` 须计入推理 token（factuality 512、curriculum 判官 256、PRM 512）。例外：`step_scorer_prm`（本地 GBNF）、`synthetic_eval_gen`（固定 budget 层）、`rag_retrieval` 查询分解取 Disabled。
- 精确匹配响应缓存（`internal/llm/response_cache.go`，表 `047_llm_response_cache.sql`）：仅当 T=0、Disabled、无 tools、Purpose ∈ {`graphrag_extract|summary|community|concept`、`rag_summary_tree`、`rag_query_rewrite`、`memory_write_filter`} 且 finish_reason 完整、非空、无 tool_calls；键 = sha256(provider|模型|purpose|response_format|max_tokens|top_p|规范化 messages)；默认 7 天，条数有界；命中记 `llm_calls.status=cache_hit`。
- 内核阶段（perceive/plan/reflect/respond/validate）禁用任何响应缓存。`search.SemanticCache` 虽在 `boot_substrate.go` 装配进路由，但 `SemanticCacheHints` 无人填写，实际不生效，维持不启用。已知局限：命中发生在记录包装内，路由 `recordAttempt` 仍记成功，半开熔断探测命中缓存会误判恢复（代价有界）。
- 重新评估：精确缓存命中率连续 30 天 <5% → 删除缓存与表；判官降 Low 后 Eval 一致性下降 → 调回 high。

## 决策八：错峰调度（原 ADR-0105 决策七）

- `m1_router.offpeak.windows`（UTC 窗口列表，默认空=不错峰；不硬编码厂商时刻表，`pkg/offpeak`）。可延迟用途（`graphrag_*`、`synthetic_*`、`curriculum_*`、`prompt_optimizer`、`rag_summary_tree`）在窗口外推迟：outbox 类置 `next_retry_at` 为窗口起点（`protocol.OffPeakDeferral`，不计失败）；周期循环类跳过 tick；`PromptOptimizer.OptimizeTask` 登记待办。
- 不错峰：交互路径、`consolidate_summary`、`TriggerCurriculum`、红队 24h 探针、`logic_collapse_codegen`、`memory_write_filter`/`rag_query_rewrite`。

## 决策九：用量账本与可见性（原 ADR-0101 决策六、原 ADR-0105 决策八）

- 表 `llm_calls`（`039_llm_calls.sql`）每次 Provider 调用一行：会话、用途（`types.WithPurpose`，未标注记 `unspecified`）、Provider、模型、池、思考档、流式、状态、input/cache_hit/output/reasoning token、耗时、估算费用、错误。写入点 `usageRecordingProvider`（`internal/llm/usage_recorder.go`，覆盖路由主路径、failover、降级池、溢出升级与 `PickProvider`），有界异步队列 512（满丢并告警）。`ProviderRegistry.Get` 返回原始实例。OpenAI 兼容 usage 经 `OpenAIUsage.toUsage` 解析 DeepSeek 顶层缓存与推理 token。DeepSeek 适配器按模型填官方费率（高峰价）。
- `input_tokens` 口径归一为含缓存命中：Anthropic 的 cache_read/cache_creation 在 `fillUsage` 加回（creation 按输入费率，估算取下界）；归一前历史 Anthropic 行为旧口径，API 层把命中率夹到 1。
- 可见性：`GET /v1/usage`（非 `/api/v1`；按时间段/purpose/model 聚合，含缓存命中率与估算费用）、`polaris usage`（经 HTTP，ADR-0096）、OTel 指标 `polaris.llm.input_tokens_total` / `polaris.llm.cache_hit_tokens_total{purpose,provider}`（不导出 ratio gauge）。保留期 `m1_router.usage.retention_days`（默认 90，0=不清理），挂 6h 清理周期，分批 DELETE 5000 行。

## 决策十：回合调用数压缩与寒暄快路（原 ADR-0102 决策一、原 ADR-0102 决策四）

- 寒暄快路：`fsm.ClassifyIntentWeight`（纯规则，≤16 rune，整句精确匹配）分 `IntentPhatic` / `IntentAck` / `IntentFull`。Phatic 在 S_IDLE→S_PERCEIVE 以 `DeterministicEffect` 直达 `S_PERCEIVE_DIRECT`（route=`phatic_bypass`），跳过 Perceive LLM 与全部召回，只调一次不挂工具的 Respond；Ack（好的/ok）**不得**直答（可能授权上一轮动作），仍走 Perceive 但跳过长期记忆召回。宁漏判不误判；Phatic 只能到达不挂工具的 S_RESPOND。
- 4a 跳过 Reflect：首轮（replanCount=0）+ `ExecAllSucceeded` + 0<Complexity<`reflect.skip_complexity`(0.4) → `trySkipReflect` 直达 `S_REFLECT_DONE`（route=`reflect_skipped`）；复杂度缺失/偏高、有失败、重规划轮次保留 LLM 反思。
- 4b′ 直答合并：`NeedsTools=false` 时 Perceive 输出可选 `Reply`，经 `publishableReply` 拦截内部产物后存 `StateContext.PreparedReply`；仅 Perceive→Respond 入边（`directRespondEffects`）消费，Effect 内以用户受众发布，S_RESPOND 仍是唯一向用户发布正文的状态（par_inv_06）。Reply 空/被拦截 → 照常 Respond。代价：直答不再逐 token 流式。
- 驳回：用小模型/embedding 分类寒暄；Perceive 并入 Plan（Plan 面向内部受众，ADR-0098）；Reflect 并入下一轮 Plan；Ack 纳入直答旁路。
- 重新评估：`phatic_bypass` 负反馈率显著高于 direct → 收紧词表；`direct_merged` 出现内部产物泄漏 → perceive.md 去 `Reply`；`reflect_skipped` 用户重问率显著偏高 → 调低 `skip_complexity`。

## 决策十一：请求边界字节稳定（原 ADR-0105 决策十一）

- PII 令牌：`PIITokenVault.TokenizeForTask` 同 taskID 内同原文映射同令牌（反向映射复用），跨会话随机；不规范化大小写/空白（保守，合并不可逆）；生成时检测碰撞并重试。映射仅内存、不持久化（`task_pii_vault` 只快照原文），重启后冷缓存；回合终态用 `ReleaseTask`（会话生命周期，6h 空闲/1024 会话回收）而非每回合 `ClearTask`。
- 真实请求边界门控 `internal/agent/request_prefix_gate_test.go`（录制式假 Provider，真实 FSM/ImmutableCore/PII 检测）：同回合 L0..L2 字节一致、相邻未跳窗回合互为严格前缀、tools 跨回合只追加、无旁路召回、同一 PII 同令牌；含 8 个负向子测试。凡在 PromptFn 之后改写消息的步骤（令牌化、压缩、溢出恢复、PRM 候选）都在其覆盖下。

## 已驳方案

| 方案 | 驳回理由 |
|---|---|
| 适配器层把「未指定档位」统一改 disabled | 同时改变 OpenAI 兼容透传与面向用户回复；按调用点声明可审计、可回滚（0101） |
| 回合内合并 Perceive/Plan/Reflect 为单次调用 | 与 ADR-0098 阶段契约及 HE-5 冲突；改以共享前缀降低拆分代价 |
| 内核回复语义缓存 | 依赖会话状态/工具结果/安全门，跨会话复用有泄漏与过期风险 |
| Agent 自主上下文管理（DTOC `manage_context`） | 收益因模型而异（部分 token 反增 18~49%），且策略交给不可信输出 |
| KV-cache 压缩（ActKV/StepKV） | 仅适用自托管引擎，主力为托管 API |
| 统一各阶段思考档位以保 Anthropic messages 缓存 | 档位是质量/成本主杠杆，主力 DeepSeek 不受影响 |

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-09-25 | 原 ADR-0101 初稿：关停无主后台回合、每回合一次蒸馏、阶段档位与模型池、稳定/易变分层、工具 schema 单次下发、`llm_calls` |
| 2026-09-25 | 原 ADR-0102：寒暄快路、跳过 Reflect、直答合并、按成因升级；原决策二（污点/复杂度驱动）并入决策六；收紧 0101 决策五为「文本目录只列名」 |
| 2026-09-29 | 原 ADR-0105 初稿（Proposed）：五层账本、历史只追加、召回预算、调用门控、响应缓存、错峰、用量可见性；起因是审计发现工具名 map 随机序使稳定层逐请求变化 |
| 2026-09-30 | 追加决策九/十/十一；WP4–WP11 全部落地，实施追记并入各节。勘误：「持久化恢复 PII 反向映射」作废（映射从不持久化）；WP7「契约追加在稳定层末尾」被 WP10 的独立首条契约 system 取代；删除第二条召回管线 |
| 2026-10-12 | 三份合一为本文；状态按代码核实改为 Accepted；重排门标注生产未接线 |
| 2026-10-18 | 决策二追记复核（ADR-0115）：推翻「选档归程序」，模型自主选档与思考档；决策一追记 full 档默认开后台任务 |

## 引用代码

- 门控/账本：`internal/llm/usage_recorder.go`、`internal/llm/response_cache.go`、`internal/store/repo/repo_llm_call.go`、`internal/protocol/schema/039_llm_calls.sql`、`internal/protocol/schema/047_llm_response_cache.sql`、`tools/llm_call_opts_lint.go`、`pkg/types/purposes.go`、`pkg/offpeak/offpeak.go`、`internal/gateway/server/sysadmin/usage.go`、`cmd/polaris/cli_usage.go`
- 阶梯/快路：`internal/agent/fsm/escalation.go`、`internal/agent/fsm/intent_gate.go`、`internal/agent/fsm/transitions_respond.go`、`internal/observability/metrics/metrics_handler.go`、`internal/agent/agent_lifecycle.go`
- 前缀/历史/召回：`internal/prompt/prompt_builder.go`、`internal/agent/fsm/prompt_layers.go`、`internal/agent/fsm/prompt_respond.go`、`internal/memory/store/immutable_core_prompt.go`、`configs/phase_contracts.go`、`internal/agent/agent_context_compaction.go`、`internal/agent/context/recall.go`、`internal/agent/context/recall_fuse.go`、`cmd/polaris/adapters_recall.go`、`internal/agent/request_prefix_gate_test.go`
- 适配器/工具：`internal/llm/adapter/anthropic_request.go`、`internal/llm/adapter/local.go`、`internal/agent/context/tool_list_section.go`、`internal/security/guard/pii_token_vault.go`
- 阈值：`docs/arch/spec/state.yaml`（`§thresholds.m4_kernel`、`m1_router`）
