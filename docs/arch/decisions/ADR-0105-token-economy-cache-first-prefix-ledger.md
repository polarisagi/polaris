# ADR-0105: Token 经济第三批——缓存优先前缀分层（Prefix Ledger）、历史只追加、召回预算、调用参数门控、确定性响应缓存、错峰调度

- **状态**: Proposed
- **日期**: 2026-09-29
- **决策者**: 架构组
- **相关模块**: M01（internal/llm）/ M04（internal/agent、internal/agent/fsm、internal/agent/context）/ M05（internal/memory）/ internal/prompt / internal/tool/catalog / internal/gateway/server/chat / M09 / M10
- **前置**: ADR-0101（阶段思考档位、模型池、稳定层/易变层、`llm_calls` 记账）、ADR-0102（寒暄快路、直答合并、按成因升级、Anthropic 稳定前缀断点）。本 ADR 是其第三批增量，承接 ADR-0101「未在本 ADR 落地」表中的"会话历史可缓存化""召回记忆位置""其余后台调用点的用途标注""llm_calls 保留期"四项，不重复前两份的决策。

## 上下文

2026-09-29 代码审计（main @ 45ac2e2）发现，ADR-0101 决策四"稳定层跨问题、跨阶段字节不变"在代码上**不成立**，且缓存前缀在稳定层之后即被打断：

1. **稳定层不稳定（P0 缺陷）**。`internal/gateway/server/chat/system_prompt.go` 以 `s.ToolReg.List()` 拼接 `ImmutableCore.BuiltinTools`；`InMemoryToolRegistry.List()`（`internal/tool/tool.go`）遍历 `map[string]types.Tool`，顺序每次随机。工具名位于 `configs/prompts/system_prompt.md` 第 10 行——稳定层第一条 system 消息在前几十个 token 处就逐请求变化，其后全部内容（身份、指令、偏好、全部阶段 prompt）对 DeepSeek/OpenAI 前缀缓存永不命中。同文件 `queryPluginSummary` 的 SQL 无 `ORDER BY`，且把 MCP 连接状态（✓/~/✗）写进稳定层。
2. **易变层位于第 2 条消息**。`ImmutableCore.PrependToMessages` 把 `# VOLATILE CONTEXT`（日期 + 按本轮问题挑选的 AmbientContext 技能全文）紧贴稳定层之后输出，其后的阶段指令、核心记忆、对话历史全部随问题变化而失配。
3. **阶段指令先于会话内容**。`PromptBuilder.Build` 顺序为 ZoneImmutable（含阶段模板 perceive/plan/reflect/respond.md）→ CoreMemory → MutableSkill → ExternalCatalog → TaintedData。四个阶段模板互不相同，因此同一回合的 4 次调用只共享稳定层；核心记忆、工作区上下文、扩展目录、对话历史在每个阶段都按未命中价重付。
4. **对话历史是单条滑窗消息**。`fsm.WriteConversationHistory` 把最近 20 条/24KB 渲染成一条 `<conversation_history>` 用户消息，窗口逐条滑动，且整段经 `taint.Spotlighting` 以整段 sha256 作围栏标记——每轮必然字节不同，永不命中缓存（ADR-0101 第二批第一项，仍未落地）。
5. **召回无预算、Perceive 与 Plan 重复召回**。`agent/context/recall.go` 的情景记忆直接输出 `pbEv.Payload` 原始 JSON、RFC3339 秒级时间戳，无单条截断、无相关度阈值、无总预算；Perceive（按 intent，K=3）与 Plan（按 goal，K=5）各自完整召回一遍（episodic + reflection + L2 语义 + RAG，含 embedding）。召回段位于对话历史之前（ADR-0101 第二批最后一项，仍未落地）。
6. **27 个 Provider 调用点未声明用途、25 个未声明思考档位**：`swarm/planner`（pool/decomposer）、`extension`（skill/plugin creator、mcp_manager_net）、`security/guard/factuality_guard`、`learning`（optimizer、curriculum×3、logic_collapse、synthetic×2）、`agent/prm`、`agent/step_scorer_prm`、`action/lam`、`knowledge/rag_retrieval`（查询改写）、`eval/analysis`（shadow×2、sampling_scorer）、`eval/harness/runner_eval`×2、`cmd/polaris/boot_tools.go`。DeepSeek 省略即 `effort=high`；其中判官类调用 `MaxTokens` 仅 64，高档推理可能耗尽上限而输出为空，付了推理费拿不到结果。`llm_calls` 中这些调用全部记为 `unspecified`，无法归因。
7. **`SemanticCacheHints` 全仓无人填写**：`router.go` 已接线 `search.SemanticCache`，但 `CacheHints` 恒为 nil，缓存从未生效；也不存在面向确定性后台调用（相同输入必得相同输出）的精确匹配缓存——GraphRAG 抽取、查询改写、记忆写入过滤对同一文本反复付费。
8. **懒加载工具激活按名称重排整个 tools 数组**（`catalog/composite.go` `Schemas` 走排序后的 `List`）：某轮 `search_tools` 激活一个工具，插在字母序中间，tools 定义从该处起失配；Anthropic 上 tools 变化使**整个**缓存失效。
9. **无错峰**：可延迟的后台批任务（GraphRAG 建图/社区摘要、合成评测、课程、Prompt 优化器）与交互流量同价执行。

外部依据（2026-09 检索）：

- DeepSeek V4 前缀缓存：缓存单元在请求边界、公共前缀检测点与固定 token 间隔产生，`A+B → A+B+C` 可复用完整前缀单元；命中价约为未命中的 1/50；另有非高峰时段约半价（chat-deep.ai 实测文档 2026-09；api-docs guides/kv_cache）。
- OpenAI Prompt caching：`prompt_cache_key` 在 GPT-5.6 之前的模型上是提高命中率的必要路由键；`prompt_cache_retention` 可选 `in_memory`/`24h`；最佳实践"历史只追加、tools 定义与顺序稳定（用 `allowed_tools` 收窄而非改 tools）、易变内容置后"（developers.openai.com guides/prompt-caching）。
- Anthropic Prompt caching：请求级 `cache_control` 自动缓存（断点随对话前移），显式断点 ≤4，TTL 5m（写 1.25×）/1h（写 2×），读 0.1×；**tools 变化使全部缓存失效**，system 变化使 system+messages 失效，思考参数变化使 messages 失效；Opus/Sonnet 5.5 最小可缓存 512 token（platform.claude.com build-with-claude/prompt-caching）。
- TokenPilot（arXiv 2606.17016）：以静态占位替换易变字段保证"首轮起字节一致的前缀"+ 按任务生命周期批量驱逐而非逐轮分页，两基准成本 −61%/−56%（持续模式 −61%/−87%）。
- "Don't Break the Cache"（arXiv 2601.06007）：稳定前置、易变后置，API 成本 −41~80%。
- DTOC（arXiv 2609.26121，2026-08）：工具输出外置 + 占位符，且**可逆**（可重新展开）是关键；但 Opus/Gemini 类模型反而多耗 18~49% token——Agent 自主管理上下文的收益因模型而异。
- "The Complexity Trap"（arXiv 2508.21433）：观察遮蔽与 LLM 摘要同效且便宜约 50%（ADR-0100/0102 已采纳，本 ADR 延续）。
- "Beyond Token Savings: A Systematic Study of Context Compression in LLM Agents"（arXiv 2609.32961）、"Compress What You See, Not What You Say"（arXiv 2609.31430）：压缩须计入缓存失效与额外步数的隐性成本（本次检索时 arXiv 限流，仅据摘要，结论未作决策依据）。

## 决策

**把 prompt 按"变化频率"重排为五层前缀账本，使同一回合的全部阶段与后续回合共享最长字节一致前缀；对话历史只追加、分块跳窗；召回设预算并在回合内复用；每个 Provider 调用必须显式声明用途与思考档位（门控）；确定性后台调用走精确匹配响应缓存；可延迟后台任务错峰执行。**

### 决策一：五层前缀账本（Prefix Ledger）

所有内核阶段（Perceive/Plan/Reflect/Respond）与记忆/降级两条路径的消息顺序统一为：

| 层 | 内容 | 变化频率 | 角色 |
|---|---|---|---|
| L0 稳定核 | ImmutableCore 稳定层（身份、指令、平台、工具名、偏好） | 部署/配置变更 | system |
| L1 会话层 | 核心记忆块、子 Agent 画像、工作区上下文（可信/不可信）、扩展目录 | 会话内低频 | system（围栏规则不变） |
| L2 历史层 | 对话历史，逐条真实消息，只追加（决策二） | 每回合追加 | user/assistant（TaintHigh 围栏逐条） |
| L3 阶段层 | 阶段契约模板 + 易变层（日期、AmbientContext、上下文压力提示、预算约束、ToolHints） | 每阶段 | system |
| L4 回合层 | 召回、TaskModel、GroundingGap、观测、重规划反馈、执行结果、本轮意图 | 每调用 | user |

字节稳定规则（违反即缺陷）：

1. L0/L1 内任何集合渲染必须有确定序（排序或 SQL `ORDER BY`），禁止遍历 map 直接拼接；`InMemoryToolRegistry.List()` 返回按名称排序的结果。
2. L0/L1 禁止出现随时间/连接状态/资源用量变化的量；MCP 连接状态标记移入 L3 易变层。
3. Spotlighting 标记按**单条消息**内容求哈希（已是现状），历史逐条围栏后，旧消息的标记不随新消息变化。
4. 污点分区语义不变：Zone 的信任级别与围栏规则保持 ADR/S-02 现状，本决策只改变**同信任级别内**与**跨 Zone 的输出次序**；阶段契约模板仍是 TaintNone 指令，只是位置后移到历史之后。`PromptBuilder` 新增按层输出的 `BuildLayered`（或等价实现），旧 `Build` 保留供非内核调用方。

门控：新增前缀不变量测试——对同一 `StateContext` 依次构造四个阶段的请求，断言 L0..L2 的消息序列字节一致；对相邻两回合断言上一回合 L0..L2 是下一回合 L0..L2 的前缀（跳窗回合除外）。

### 决策二：对话历史只追加 + 分块跳窗

- 历史以真实 user/assistant 消息逐条写入 L2，不再合并为单条 `<conversation_history>`。每条仍按 TaintHigh 围栏（数据区语义不变）。
- 窗口不逐条滑动：累计超过 `m4_kernel.conversation.history_max_messages`/`history_max_bytes` 时**一次裁掉前半**，裁掉的部分由压缩摘要（已有 `agent_context_compaction.go` / ADR-0100 确定性修剪链路，优先确定性摘要）以单条锚定消息置于 L2 首位；摘要只在跳窗时重算。跳窗频率 = 1/(窗口一半)，其余回合 L2 纯追加。
- 回合内四个阶段看到的 L2 完全相同（Plan/Reflect 此前不带历史：改为带同一 L2——L2 是缓存命中价，净效果是把"每阶段不同的前缀"换成"共享前缀"；若 `llm_calls` 显示 Plan/Reflect 的未命中 token 上升，则回退为不带历史，见重新评估触发条件）。

### 决策三：Provider 缓存接线

- **Anthropic**：断点改为 L0 末、L2 末、最后一条消息（≤4，tools 末尾断点仅在无 system 时使用）；新增配置 `llm.anthropic.cache_ttl`（`5m`|`1h`，默认 `5m`）。阶段间思考参数不同会使 messages 缓存失效——记录为已知代价，不为此统一思考档位。
- **OpenAI 兼容**：请求携带 `prompt_cache_key`（取 `hash(agent_id)`，同一 Agent 的请求路由到同一缓存；不含 PII）；`prompt_cache_retention` 可配（默认不传，由服务端决定）。仅对声明支持的 Provider 发送，其他兼容端点忽略未知字段的行为不作假设——按 Provider 能力位开关。
- **DeepSeek**：自动前缀缓存无需参数；本 ADR 的收益主要来自决策一/二。
- **Google**：依赖隐式缓存；确认 `cachedContentTokenCount` 写入 `Usage.CacheHitTokens`。
- **tools 数组只追加**：懒加载模式下，核心工具按名称排序在前，本会话激活的工具按**激活顺序**追加在后，不再与核心工具混排；`search_tools` 固定在核心工具之后、激活工具之前。
- **实验开关 `m4_kernel.cache.uniform_tools`（默认 false）**：为真时 Perceive/Reflect/Respond 也下发与 Plan 相同的 tools 并设 `tool_choice=none`，使四阶段 tools 前缀一致。收益/代价须以 `llm_calls` 实测（缓存命中 token 与未命中 token 对比）决定是否默认开启，不凭推断开启（HE-4）。

### 决策四：召回预算与回合内复用

- 渲染：情景记忆取事件摘要字段（无摘要字段时截断 payload），单条上限 `m4_kernel.recall.item_max_chars`，时间戳到日；反思、L2 语义、RAG 同受单条上限。
- 预算：召回段总上限 `m4_kernel.recall.max_tokens`；按相关度排序截断；低于 `m4_kernel.recall.min_score` 的语义/RAG 命中丢弃。
- 去重：召回内容与 L2 历史重复（规范化后完全包含）的条目丢弃。
- 复用：Plan 复用本回合 Perceive 的召回结果（存于 StateContext，回合起点清空）；仅当 Plan 的查询词（goal）与 Perceive 的不同且召回为空时补查一次。查询 embedding 在回合内缓存。
- 召回段位于 L4（历史之后、本轮意图之前），不再打断历史前缀。
- Reflect 的执行结果与 Respond 共用 ADR-0100 的观测上限与 ToolRefOffloader 引用，不再全文注入。

### 决策五：Provider 调用参数门控

- 全仓每个 `safecall.Infer`/`Provider.Infer`/`InferStream` 调用（`internal/llm` 内部转发除外）必须显式传 `types.WithPurpose(...)` 与 `types.WithThinkingMode(...)`。新增 `tools/llm_call_opts_lint.go`（AST 判据，覆盖变参展开 `opts...` 的情形：展开的切片在同函数内可见地包含两项即合格，不可判定即报红），登记 `tools/lint-selftest.txt` 负向用例，纳入 `make lint`。
- 默认档位：机械类（摘要、抽取、打分数字、格式转换）`ThinkingDisabled`；判官/分类类（factuality、step PRM、sampling scorer、shadow 评审、查询改写）`ThinkingLow`（**须以 Eval 确认**，HE-4；不达标调回原档）；创作/规划类（任务分解、技能/插件创建、合成评测、Prompt 优化器、LAM、课程）保留 high。输出上限 `MaxTokens` 按用途显式给出。

### 决策六：确定性后台调用的精确匹配响应缓存

- 仅当调用满足：`Temperature == 0`、`ThinkingDisabled`、无 tools、`Purpose` 在白名单（`graphrag_extract`、`graphrag_summary`、`graphrag_community`、`graphrag_concept`、`rag_summary_tree`、`rag_query_rewrite`、`memory_write_filter`）时启用。
- 键 = sha256(provider 注册名 | 模型 ID | purpose | response_format | 规范化 messages)。值存 SQLite 新表（State-in-DB），带 `expires_at`（默认 7 天），条数有界（受 `bounded-cache-check` 约束）。命中时不调用 Provider，`llm_calls` 记一行 `status=cache_hit`、token 为 0。
- 内核阶段（perceive/plan/reflect/respond/validate）**禁止**使用：它们依赖会话状态与安全门，语义缓存/精确缓存都可能跨会话复用不应复用的回答。`search.SemanticCache` 维持现状（不接线），理由同此。

### 决策七：错峰调度

- 新增 `llm.offpeak.windows`（按 Provider 配置的 UTC 时间窗列表，空=不错峰）。可延迟用途（`graphrag_*` 建图、`synthetic_*`、`curriculum_*`、`prompt_optimizer`、`rag_summary_tree`）的调度器在窗口外推迟到下一个窗口开始执行；交互路径与回合终态的 `consolidate_summary` 不受影响。
- 窗口不硬编码任何厂商时刻表（厂商会调整），默认空。

### 决策八：用量可见性

- `GET /api/v1/usage`（按时间段、purpose、model 聚合：请求数、输入/缓存命中/输出/推理 token、缓存命中率、估算费用）与 `polaris usage` CLI（经 HTTP，ADR-0096 决策一）。
- Prometheus `llm_prompt_cache_hit_ratio{purpose,provider}`。
- `llm_calls` 按 `created_at` 纳入现有归档/清理周期，保留期 `llm.usage.retention_days`（默认 90）。

### 决策九：阶段契约库并入稳定核（2026-09-30 追加）

**上下文事实**：决策一把阶段模板放在 L3（历史之后），DeepSeek/OpenAI 上可行；但 Anthropic/Gemini 只接受开头的 system，WP6 把 L3 内联为 user 角色 `<system_instruction>` 块——阶段契约（输出 Schema、路由规则、安全约束，四份合计约 7.5KB）因此以 user 角色出现，遵从度低于真 system，且每阶段按未命中价付费。

**决策**：四份阶段契约（`kernel/perceive.md`、`plan.md`、`reflect.md`、`respond.md`）以固定顺序、固定标题渲染为 L0 稳定核内的 `# PHASE CONTRACTS` 段（部署期不变，全部阶段、全部回合共享，命中价计费）；L3 只保留一条几十 token 的阶段选择器（`# ACTIVE PHASE: PLAN` + "只按该契约输出"）与易变内容。

- 契约在所有 Provider 上都以真 system 出现，Anthropic/Gemini 内联块只剩选择器与易变量，遵从度问题随之消失；WP6 的 `inline_nonleading_system` 保持默认开启。
- 无 ImmutableCore 的降级路径（无 L0）仍在 L3 写入完整契约（行为不变）。
- `respond_reminder.md` 仍位于末尾（位置优势是其存在理由），不并入。
- 开关 `m4_kernel.prompt.phase_contracts_in_core`（默认 true），供回合契约评测对照；评测不达标则关闭即回到决策一的 L3 全文模板。
- 反例守护：禁止在契约段内插入任何按会话/阶段变化的文本（阶段差异只能出现在 L3 选择器）。

### 决策十：召回单一管线、秩融合与校准相关度门（2026-09-30 追加，修订决策四的阈值部分）

**上下文事实**（审计 main @ 7d4bdc8）：
1. **存在第二条召回管线**：`agent_execute_memory.go` `injectMemoryToMsgs` 在 `executeEffect` 中对 TaskModel 非空的每次 LLM 调用（Plan/Reflect/Respond 及 PRM 候选）经 `Assembler` 再召回一次（情景 + 知识，上限 2000 token），并插入到开头连续 system 之后——即 L1 与 L2 之间。它与决策四的回合内召回重复计费，且把按 Goal 变化的内容插进共享前缀中部，使 Plan/Reflect/Respond 的 L2 历史前缀全部失配。决策一的前缀门控只测了 builder，没覆盖真实请求边界，故未发现。
2. L2 语义召回 `SetCognitiveSearcher` 生产无调用点（未接线）。
3. RAG 适配器 `fsmKnowledgeAdapter.SearchRAG` 丢弃检索分，恒填 1.0。
4. 决策四的 `min_score_ratio=0.2` 无数据依据；各来源分数量纲不可比（BM25 无界、向量余弦、RAG 恒 1.0）。

**决策**：
- **单一管线**：删除 `injectMemoryToMsgs` 与 `Assembler` 这条旁路；其独有能力（按 SurpriseIndex 决定知识检索深度、按 MaxTaint 过滤）并入决策四的回合内召回。全内核只有一处召回、一处注入点（L4）。
- **秩融合取代分数阈值**：各来源先按各自原生分排序，再以 RRF（k=60）融合为单一序列，按融合秩装入 token 预算。RRF 只用秩，不需要跨来源可比的分数，因此不再需要 `min_score_ratio`（默认改为 0=关闭，保留开关）。来源优先级改为对 RRF 分的加权（反思、情景权重可配），不再是硬顺序。
- **校准相关度门（有本地重排器时）**：Tier1 已加载本地交叉编码重排器（Qwen3-Reranker，`ffi.LlamaRerank`，零 API token）时，对融合后前 N 条重排，丢弃相关概率低于 `m4_kernel.recall.rerank_min_prob`（默认 0.5——交叉编码器 yes/no 二分类的自然判定边界，而非经验比例）的条目。无重排器时不设相关度门，仅由预算约束（预算才是成本的硬上限）。
- **接线 L2 语义召回**：按 ADR-0062 WIRE 标准核实 `SurrealDBCoreStore` FTS/向量索引中存放的内容及取正文方式，实现 `fsm.CognitiveSearcher` 适配器并在 `boot_agent.go` 注入；若核实为与情景/RAG 完全重复的数据源，则改为删除该路径并在本 ADR 追记理由（二者择一，不得保持"有接口无接线"）。
- **RAG 分数透传**：`KnowledgeBase.Search` 返回检索分，适配器不再填常量。

### 决策十一：请求边界字节稳定（2026-09-30 追加）

**上下文事实**：`PIITokenVault.TokenizeForTask` 对每次出现的 PII 生成新的随机令牌，`tokenizeMessagesForLLM` 在每次 LLM 调用前对全部消息重新令牌化——凡含 PII 的消息（用户画像、核心记忆、历史）每次请求字节都不同，L0..L2 前缀在该位置断开。

**决策**：
- **确定性会话内令牌**：同一 taskID（会话）内同一原文映射到同一令牌（反向映射复用），跨会话仍随机（不可关联性不变）；持久化/恢复（ADR-0104 `task_pii_vault`）同时恢复反向映射。令牌格式与解析语义不变。
- **真实请求边界门控**：新增端到端测试——以记录请求的 fake Provider 驱动一个完整回合（Perceive→Plan→Execute→Reflect→Respond，含 PII、核心记忆、召回、历史）及相邻第二回合，断言**实际发往 Provider 的消息**满足：同回合各阶段 L0..L2 字节一致；相邻回合（未跳窗）前缀关系成立。凡在 PromptFn 之后改写消息的步骤（令牌化、压缩、溢出恢复、PRM 候选）都在此门控覆盖下。

## 后果

- **正向**：稳定层恢复为真正的字节稳定；同一回合 4 次调用共享 L0..L2，跨回合 L2 只追加——对话越长，命中占比越高；召回段有上限且不重复计费；每一笔调用可归因；确定性后台重复计算归零；可延迟批任务可按非高峰价执行。
- **负向**：阶段模板后移到历史之后，指令与输出之间的距离变短（通常有利于遵循），但模型对"系统指令出现在对话中部"的遵循度需以回合契约评测确认；Plan/Reflect 新增携带 L2 历史，在缓存未命中时（冷启动、跳窗回合）会多付历史 token；精确缓存引入一张表与失效逻辑；lint 门控使新增调用点必须多写两个选项。
- **反例守护**：
  - 在 L0/L1 中遍历 map、查询无 `ORDER BY`、写入时间/状态/资源量——引用本 ADR 拒绝。
  - 把按请求变化的内容放到历史之前；把历史重新合并为单条滑窗消息——拒绝。
  - 不带 `WithPurpose`/`WithThinkingMode` 的新 Provider 调用——门控拦截。
  - 对内核阶段启用任何响应缓存（精确或语义）——拒绝。
  - 为提高缓存命中而放宽污点围栏或把数据区内容提升到指令区——拒绝（缓存让位于 HE-2/HE-7）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 在热路径上用 LLM/小模型做 prompt 压缩（LLMLingua 类） | 额外调用 + 延迟；压缩结果逐请求不同，破坏字节稳定前缀，抵消缓存收益（Complexity Trap / "Beyond Token Savings"） |
| 对内核回复启用语义缓存（相似问题直接返回旧答案） | 回答依赖会话状态、工具结果与安全门；跨会话复用可能泄漏或给出过期结论；Agent 场景命中率低 |
| 引入 Agent 自主上下文管理工具（DTOC `manage_context`） | 收益因模型而异（部分模型 token 反增 18~49%），且把上下文策略交给不可信输出；与 ADR-0102"LLM 给信号、程序持策略"一致，暂不引入，待确定性分层落地后以 Eval 再议 |
| KV-cache 压缩（ActKV / StepKV 类） | 只适用于自托管推理引擎；本项目主力为托管 API，Tier1 本地推理由 llama.cpp 自带前缀缓存覆盖 |
| 合并 Perceive/Plan/Reflect 为单次调用 | 已由 ADR-0101/0102 驳回（HE-5 阶段契约）；本 ADR 以共享前缀降低阶段拆分代价 |
| 把全部阶段的思考档位统一以保 Anthropic messages 缓存 | 思考档位是 ADR-0101/0102 的质量/成本主杠杆，价值高于 Anthropic 阶段间 messages 缓存；主力 Provider（DeepSeek）不受此影响 |

## 引用代码

- `internal/tool/tool.go` `InMemoryToolRegistry.List`、`internal/gateway/server/chat/system_prompt.go`、`internal/gateway/server/chat/system_prompt_extensions.go`（决策一，稳定层缺陷）
- `internal/memory/store/immutable_core_prompt.go` `PrependToMessages`、`internal/prompt/prompt_builder.go` `Build`、`internal/agent/context/memory_context.go`、`internal/agent/context/respond_context.go`、`internal/agent/fsm/state_machine_prompts.go`、`internal/agent/fsm/prompt_respond.go`（决策一/二）
- `internal/agent/fsm/conversation.go` `RenderConversationHistory`、`internal/agent/agent_context_compaction.go`（决策二）
- `internal/llm/adapter/anthropic_request.go`、`internal/llm/adapter/client.go`、`internal/llm/adapter/openai.go`、`internal/llm/adapter/google.go`、`internal/tool/catalog/composite.go` `Schemas`、`internal/agent/agent_execute_effect.go`（决策三）
- `internal/agent/context/recall.go`（决策四）
- `internal/llm/safecall/safecall.go` 及上下文第 6 条所列 27 个调用点（决策五）
- `internal/llm/router.go` `resolveSemanticCache`（决策六，维持不接线）
- `internal/protocol/schema/039_llm_calls.sql`、`internal/llm/usage_recorder.go`（决策八）
- `docs/arch/spec/state.yaml §thresholds.m4_kernel`（新增阈值）

## 重新评估触发条件

1. 真实 Key 回合契约评测中，阶段模板后移导致 Perceive 路由或 Plan schema 失败率较调整前上升 >2 个百分点：阶段层前移到 L1 之后、L2 之前（放弃阶段间共享 L2，保留跨回合共享）。
2. `llm_calls` 显示 Plan/Reflect 携带 L2 后，其未命中输入 token 的周均值上升且总费用上升：Plan/Reflect 回退为不带历史。
3. 判官类调用降为 `ThinkingLow` 后，Eval 判定一致性下降：该用途调回 high。
4. 精确缓存命中率连续 30 天 <5%：删除该缓存与表（不为无收益机制保留代码）。
5. 主力 Provider 转为不对前缀计费差异（命中价/未命中价 > 1/3）：决策一/二的收益重估。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-09-29 | 初稿（Proposed） |
| 2026-09-30 | 追加决策九（阶段契约库并入稳定核）、决策十（召回单一管线/RRF/校准重排门/接线 L2，修订决策四阈值）、决策十一（PII 确定性令牌 + 真实请求边界门控） |
| 2026-09-30 | WP4 落地（决策五、六）：Purpose* 常量集中于 `pkg/types/purposes.go`；`tools/llm_call_opts_lint.go`（L-19）门控；`047_llm_response_cache.sql` + `internal/llm/response_cache.go`（接入 `usageRecordingProvider`，经 `ProviderRegistry.InjectResponseCache`）。实施偏差与补充见下「WP4 实施追记」 |
| 2026-09-30 | WP6 落地（决策一/三，Anthropic/Gemini 适配器）：新增 `m1_router.anthropic.inline_nonleading_system` / `m1_router.google.inline_nonleading_system`（默认 true）。开启时仅**开头连续**的 system 消息进 `system`/`systemInstruction`，其后的 system（L3 阶段层）原位转 user 角色 `<system_instruction>\n…\n</system_instruction>` 文本块，与相邻 user 内容合并以满足 user/assistant（Gemini：user/model）交替；tool_result/functionResponse 块前置于合并后的 user 轮首，不破坏与 tool_use/functionCall 的相邻关系；`CacheBreakpoint` 落在被合并消息对应的内容块上（末条与层断点同处一条消息时占两个名额，总数仍 ≤4）。适配器对所有非内联来源的 user 文本/tool_result 字符串无条件转义 `<system_instruction>` 标签字面（全角＜）——`taint.Spotlighting` 仅对 TaintMedium+ 生效，TaintLow/None 的 user 输入与 Parts 不经围栏。关闭时请求体与改动前字节一致（有回归测试）。实现见 `internal/llm/adapter/{inline_system,anthropic_inline}.go`、`google_request.go` |
| 2026-09-30 | WP5 落地（决策七、八）：`GET /v1/usage` + `polaris usage`；`pkg/offpeak` 错峰窗口；llm_calls 保留期。见下「WP5 实施追记」 |

### WP4 实施追记（2026-09-30）

- **判官 `MaxTokens` 同步放宽**：推理 token 计入 max_tokens，ThinkingLow 下 8/64/128 仍会被耗尽。factuality 64→512、curriculum SIC/安全判官 8→256、PRM 打分 128→512；判定按前缀解析，放宽上限不改判定逻辑。
- **三处不按决策五表降档**（理由写在各调用点注释）：`agent/step_scorer_prm.go`（本地 GBNF 模型，上限 4 token / 100ms 硬超时）与 `learning/synthetic/synthetic_eval_gen.go`（已固定 budget 层 deepseek-chat，512 上限）取 Disabled；`knowledge/rag_retrieval.go` 查询分解取 **Disabled 而非 Low**——决策六缓存白名单含 `rag_query_rewrite`，而缓存只对 Disabled + T=0 生效，定为 Low 则该白名单项永远不可命中。
- **缓存键加入 `max_tokens` 与 `top_p`**（决策六原五要素之外）：二者改变输出，不纳入则小上限调用会复用大上限的结果。仅缓存 finish_reason 为完整结束、非空、无 tool_calls 的响应。
- **缓存命中不经路由健康统计的隔离**：命中发生在 Provider 记录包装内，路由的 recordAttempt 仍会把它记成一次成功；半开熔断探测恰好命中缓存时会被误判恢复（后续真实调用失败会重新打开熔断，代价有界）。若 `llm_calls` 显示该场景实际出现，再把命中判定上移到路由选中 Provider 之后、recordAttempt 之前。
- **`agent_execute_effect.go` 的 `plan_prm_candidate` 字面量未改常量**（属 WP2 范围，避免并行改动冲突），值与 `types.PurposePlanPRMCandidate` 相同；WP2 合入后应顺手替换。

### WP5 实施追记（2026-09-30）

- **路径为 `/v1/usage` 而非 `/api/v1/usage`**：仓库全部 HTTP 路由在 `/v1/` 下（`/api/` 前缀仅被 SPA 回退当作"API 不回退"，未注册任何路由），沿用现有约定。
- **`llm_calls.input_tokens` 口径归一**（决策八的"缓存命中率"前提）：OpenAI/DeepSeek/Google 的 `toUsage` 给出含命中的 prompt_tokens；**Anthropic 的 `input_tokens` 不含 cache_read/cache_creation**。此前直接落库，导致 Anthropic 行 cache_hit > input、输入费用被 `max(input-hit,0)` 夹成 0 漏算。`usageRecordingProvider.fillUsage` 现在对 Anthropic（按适配器类型名识别，`CacheCreationTokens>0` 兜底）把 cache_read/cache_creation 加回 input，creation 按输入费率计（实际 1.25x，估算取下界）。归一落地前的历史 Anthropic 行仍是旧口径，API 层把比率夹到 1。适配器（`internal/llm/adapter/`）未改。
- **Prometheus**：导出 `polaris_llm_input_tokens_total` / `polaris_llm_cache_hit_tokens_total{purpose,provider}` 两个 counter（沿用 `polaris_` 前缀，非 `llm_` 前缀），在 usage sink `push` 处打点（终态行，流式不重复计数）。不导出 ratio gauge。
- **配置键落在 `m1_router` 段**（与 `response_cache.*` 同处）：`offpeak.windows`（默认空）、`usage.retention_days`（默认 90，0=不清理）。保留期清理挂在既有 6h 周期，并在启动时先跑一次；分批 DELETE（5000 行/批）避免长时间独占单写连接。`llm_response_cache` 过期清理已在同一周期内（WP4）。
- **错峰接入点**：outbox 类（新增 `protocol.OffPeakDeferral`，outbox 直接把 `next_retry_at` 置为窗口起点，不计失败/死信）——`graph_build`、`rag_doc_ingested`、`rag_doc_summary_needed`（graphrag_*）、`m9_capability_gap`（synthetic_skill_gen）；周期循环类（窗口外跳过 tick，不积压）——synthetic-eval-gen、`curriculum` 后台调度、learning.Engine 中环；`PromptOptimizer.OptimizeTask`（窗口外登记待办并合并同 taskType，到窗口起点执行）；`DefaultIngestionPipeline` 降级 goroutine 的 rag_summary_tree（等窗口）。
- **未接入**：交互路径与 `consolidate_summary`（决策明文排除）；`TriggerCurriculum`（安全冻结/管理员显式触发，是响应事件）；红队 24h 探针（安全边界退化检测，非批处理）；`logic_collapse_codegen`（不在决策七清单，且由工具成功阈值事件触发）；`memory_write_filter`/`rag_query_rewrite`（在交互路径上）；`internal/prompt` 的 `Manager.Optimize` 未改（按任务约束），错峰在其下游 `PromptOptimizer.OptimizeTask` 生效。
