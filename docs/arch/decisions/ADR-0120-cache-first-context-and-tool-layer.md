# ADR-0120: 缓存优先的上下文引擎与工具层——前缀稳定、历史内激活工具、会话级记忆快照、跨回合目标

- **状态**: Accepted
- **日期**: 2026-10-21
- **决策者**: MrLaoLiAI（设计：Claude；实现：Sonnet）
- **相关模块**: M01（`internal/llm`）/ M04（`internal/agent`、`internal/agent/fsm`、`internal/prompt`）/ M05（`internal/memory`、`internal/agent/context`）/ M07（`internal/tool`、`internal/tool/catalog`）/ M13（`internal/gateway`）
- **修订**: ADR-0119 决策二（"激活后下一轮出现在 tools 数组尾部"→ 默认改为历史内激活）；ADR-0115 T3（观察遮蔽改为成本门控）；ADR-0105 决策一（稳定层增补"会话内冻结"规则）；M05 PersonaRefiner 的触发时机与注入位置

## 上下文

### 钱花在哪（2026-10-21 本机 `~/.polaris/data/polaris.db` llm_calls 实测，purpose=kernel、input>0 的 29 次调用）

| 项 | token | 单价（deepseek-flash，USD/M，`internal/llm/pricing`） | 费用占比 |
|---|---|---|---|
| 输入·缓存未命中 | 126,126 | 0.30 | **≈70%** |
| 输出（含推理 5,016） | 11,595 | 1.20 | ≈26% |
| 输入·缓存命中 | 368,128 | 0.006 | ≈4% |

命中价是未命中价的 1/50。15 个常驻工具（2,464 token，`TestFirstRoundBudget` 实测）在命中价下约占总费用 0.8%。结论：**再削减常驻工具几乎不省钱**；把常驻工具换成"只留一个搜索工具"还会每次多一次模型往返（整段上下文重发一次 + 额外输出 + 延迟）。真正的杠杆是**缓存未命中**与**输出**。

### 未命中从哪来（同一账本逐调用核对）

1. **跨回合前缀失效**：新回合首调几乎不命中（turn_dd4 首调 4,712 只命中 512；新会话首调 13.7K 只命中 128——连跨会话完全相同的系统提示 + 工具都没命中）。根因：`agent_lifecycle.go` 每回合结束都调 `PersonaRefiner.RefineAtSessionEnd`（输入甚至只是 `[task completed successfully, replan_count=0]`），LLM 重写 `interaction_summary`；`system_prompt.go` 每次请求把 `ToUserPreferences()`（含 `interaction_summary`）写入 `ic.UserPreferences`，渲染在**稳定层系统提示前部**的 `# CONSTRAINTS & PREFERENCES`（且措辞为 "Must strictly adhere"）。于是每回合稳定层字节变化，其后全部内容按未命中计费。
2. **回合内历史改写**：sess_0f2 一次调用 28,900 输入只命中 14,208，下一次 24,612 只命中 14,976——T3 观察遮蔽每攒够 4 条就改写一批旧工具结果，不看节省是否覆盖重建缓存的代价。按 1/50 价差，遮蔽 4K token 需要后续约 50 次调用才能回本，而一次改写立刻损失其后全部历史的命中。
3. **会话层逐调用重渲染**：`WriteSessionLayer` 每次调用 `ListCoreMemory`，回合内 `core_memory_edit` / `memory_page_*` 会改写会话层，其后历史全部失效。
4. **工具激活改写 tools 数组**：ADR-0119 的激活把工具追加到 tools 数组尾部，tools 位于消息之前，一次激活即让全部历史失效。

### 能力侧缺口（从前沿模型的工作需要出发）

- 历史窗口 20 条消息 / 24KB，且上一回合的工具轨迹不进入下一回合：模型不记得自己读过哪些文件、在哪找到的结论，只能重读——既降能力又增加未命中输出。缓存下保留历史的代价只是命中价。
- `read_file` 没有 offset/limit：读大文件只能整篇进上下文（未命中价一次性付清，之后每次调用都携带）。
- 工具索引用途截断为 22 字符（`briefMaxRunes`），英文描述被截成 "Trigger an explicit re…"，模型无法判断该激活哪个。
- 没有跨会话对话检索：`messages_fts`（013_chat.sql）存在但无工具使用，且 `unicode61` 分词对中文无效。
- 没有跨回合目标：一个回合结束后，用户不发话工作就停止。长任务需要"目标 + 自动续跑 + 预算"。
- 系统提示存在相互矛盾与过时陈述（见决策五）。

### 行业现状（2026-10 查证）

- Anthropic Messages：`defer_loading: true` + 客户端工具搜索在 `tool_result` 中返回 `tool_reference`，API 在历史内联展开定义，"The prefix is untouched, so prompt caching is preserved"；建议常驻 3–5 个高频工具。
- OpenAI Responses：`defer_loading` + `tool_search`（gpt-5.4+），发现的工具注入上下文末尾；官方提示改变已加载集合会从该点起打断缓存。
- DeepSeek Harness（`deepseek-ai/deepseek-harness`，2026-10 源码）：一切皆插件；`native | ptc` 两种呈现（ptc 下模型只见 `run_code` + 生成的 SDK 声明，官方仍默认 native，理由是日常单步调用 native 最优）；走 `https://api.deepseek.com/anthropic` 并以 beta `mid-conversation-tool-changes-2026-07-01` 发送 `defer_loading` 与历史内 `tool_addition/tool_removal`；系统提示变化在支持的路由上"历史内追加"而非改写；提供 `session_search` / `session_event_*` 让模型检索自身轨迹；`create_goal/update_goal` + 续跑轮次上限。DeepSeek 官方 Anthropic 兼容文档未列出 `defer_loading`，因此本 ADR 不依赖它。

## 决策

### 决策一：前缀稳定是可执行不变量（`inv_PrefixStable`）

定义请求序列 R(t, r)（回合 t 的第 r 次内核调用，按 provider 实际请求体序列化）：

1. 回合内：R(t, r+1) 的 tools 与 R(t, r) 逐字节相等；R(t, r+1).messages 以 R(t, r).messages 去掉末尾易变消息后的序列为前缀。
2. 跨回合：R(t+1, 1) 与 R(t, last) 的 tools 逐字节相等；R(t+1, 1).messages 以"稳定层 + 会话层 + R(t, ·) 的历史层"为前缀。
3. 跨会话：同一配置下两个新会话的 R(1, 1) 在稳定层（系统提示 + tools）逐字节相等。

允许打破前缀的事件只有：用户/模型切换模型或思考档（T4）、压缩、历史窗口按步丢弃、会话层刷新点（压缩或新会话）、成本门控遮蔽。每一种都必须可归因（span 属性 `prefix_break`，见决策八）。

门控：`internal/agent/prefix_stability_test.go`（`Test_inv_PrefixStable*`）用生产组装路径 + DeepSeek/OpenAI 兼容序列化器构造三种场景并逐字节断言；场景必须包含"回合间 persona 更新""回合内 core_memory_edit""回合内 tool_search 激活""第 7 轮以后的工具结果"。

### 决策二：稳定层与会话层"会话内冻结"

1. `interaction_summary` 不再进入任何提示词（它是会话摘要，属于记忆，按需经 `memory_search`/`session_search` 取）。PersonaRefiner 只保留四个稳定维度。
2. 用户偏好在**会话首个回合**快照进会话状态，整场会话使用快照；PersonaRefiner 改为会话结束/空闲挂起时运行一次（≥3 个回合才运行），输入为真实的用户消息与助手最终回复，走 default 池、关思考、错峰。
3. 会话层（系统环境、核心记忆块、Agent Profile、工作区上下文、扩展目录、记忆索引）在会话开始时渲染一次并冻结；刷新点为压缩与新会话。`core_memory_edit`、`memory_page_in/out` 立即写库，但当前会话的可见效果来自其工具结果（历史内），下一刷新点再进入会话层。工具描述同步说明这一语义。
4. `kernel/loop.md` 从 Phase 层（历史之后）移入稳定层。
5. 易变信息分两级：**回合级**（日期、本回合相关的 ambient 技能、与上回合不同的扩展连接状态）在回合开始时生成一次，作为一条 user 消息放在本回合意图之前，回合内位置不变（因而回合内后续调用命中）；**调用级**只保留 `[usage]` 一行与告警，放在末尾。
6. 系统提示删去 `Engine: {{.ModelID}}`（无行为价值，且模型 failover 时改写稳定层）。

### 决策三：工具层——常驻核心 + 历史内激活 + 代码直达

1. **不追求零常驻**。常驻核心集（静态、字节稳定）：`read_file write_file str_replace_editor glob grep bash code_act web_search web_fetch memory_search todo_write spawn_agents tool_search call_tool`（循环模式另有 `finish`）。`list_dir`、`run_command` 降为延迟加载（`glob` 与 `bash` 已覆盖）。
2. **默认激活方式改为历史内**：`tool_search` 的结果直接返回命中工具的用途与紧凑参数 schema（进入历史、此后命中）；模型用常驻元工具 `call_tool{name, arguments}` 立即调用，无需等"下一轮"，tools 数组整场会话不变。内核在 S_VALIDATE 之前把 `call_tool` 解包为内层调用，此后的参数校验、Cedar/HITL 审批、Taint、ExecuteTool、步骤事件、指标全部以内层工具名进行，`call_tool` 本身不构成审批主体，禁止嵌套 `call_tool`。原"追加到 tools 数组"路径保留为配置 `m7_tool.lazy_activation = "tools_array"`，默认 `"in_history"`。
3. Anthropic 适配器（可选子项）：延迟工具以 `defer_loading: true` 全量声明，`tool_search` 在该 provider 下额外返回 `tool_reference` 块，模型可原生调用；其余 provider 走 2。
4. `code_act` 的 `polaris.call()` 已可调用注册表内全部工具（`internal/action/codeact/bridge.go` `Lookup`），描述明确写出这一点与 `polaris.tools(query)`；多步数据流优先写成一段程序（PTC 思想），只打印结果。不采用 DeepSeek 的"仅 run_code"呈现。
5. `read_file` 增加 `offset`/`limit`（1 起始行号），输出带行号；未指定时默认上限 2000 行 / 64KB，超出返回截断说明与总行数。
6. 工具索引每行用途改为人工维护的短句：内置工具新增 `IndexBrief`（中文 ≤24 字），缺省回退为描述首句在词边界截断至 60 字符；索引仍按类别分组、确定序。

### 决策四：历史与观察的成本模型

1. 会话历史窗口由"条数 20 / 24KB"改为 token 预算：`conversation.history_max_tokens`（默认 48000），超出时一次丢弃至 50%（锚点摘要保留），而不是每回合滑动。
2. 回合结束时向该回合的助手消息附加一段**回合摘要**（由步骤时间线机械生成，不调 LLM）：每个工具调用一行 `工具(关键参数) → 结果规模 [ref:id]`，≤ 600 字符。模型因此知道上回合读过什么、在哪能取回全文（`read_tool_ref`）。
3. 回合级记忆召回（`runLoopRecall`）保留，但加上限 `loop.recall_max_chars`（默认 2000），寒暄类短意图跳过。
4. 观察遮蔽改为成本门控：仅当本回合估计上下文 ≥ `loop.observation_mask_trigger_tokens`（默认 0 表示取模型上下文窗口的 50%，无窗口信息时 64000）时触发；只遮蔽 ≥ `loop.observation_mask_min_tokens`（默认 1024）且超过 `observation_mask_after_rounds` 的结果；一次遮蔽全部候选（一次断缓存），不再按 4 条一批。

### 决策五：系统提示重写

删除：`Engine`、空的 `OBJECTIVE`/`CONSTRAINTS` 标题、"No Conversational Filler / Output only the requested structured data / Deterministic Output / Structured Alignment"（五段式 JSON 时代残留，损害对话质量）、"Only use tools explicitly listed"（与延迟加载矛盾）、"Execution Engine Confidence… hardware-isolated Wasm sandbox… fearlessly"（与信任档位事实不符）、"All internal reasoning and tool calls remain in English"、按 Locale 回复（与 loop.md "按用户语言回复"矛盾，统一为后者）。"立即调用工具"在 identity/模板/directives/loop/model guidance 五处重复，合并为一处。新增：成本意识（缓存价差、按范围读、代码直达、子智能体隔离上下文）、记忆与对话检索的使用时机、目标续跑。正文见 `configs/prompts/system_prompt.md`、`configs/prompts/operational/directives.md`、`configs/prompts/kernel/loop.md`。

### 决策六：会话级记忆索引与对话检索

1. 会话层新增 ≤1200 字符的**记忆索引**：核心记忆块键与字数、长期记忆条数、按访问频次前 12 个实体/主题名、技能名。会话开始时生成并冻结。模型据此决定何时 `memory_search`，而不是被动注入召回。
2. 新增延迟工具 `session_search{query, session_id?, since?, limit?}` 与 `session_read{session_id, message_id?, around?}`。中文检索新增 `054_messages_trigram_fts.sql`（`tokenize='trigram'`，触发器同步 + 回填；≤2 字的 CJK 查询回退 LIKE 有界扫描）。

### 决策七：跨回合目标（goal）

新增 `053_agent_goals.sql` 与延迟工具 `goal{action: set|update|get|clear, objective?, done_criteria?, status?, note?, max_rounds?}`。会话存在 `active` 目标且回合正常结束、没有待处理的用户消息、未处于审批等待、续跑轮次 < `max_rounds`（默认 20，用户可改）且会话预算未超时，内核自动发起一个续跑回合（来源标记 `goal`，前端可见、可停止）。连续 2 个续跑回合无任何工具调用 → 自动 `paused`。模型以 `goal update status=complete|blocked` 结束。用户发消息或点停止优先于续跑。

### 决策八：可观测与计量

不改 `llm_calls` 表结构；每次内核调用在 span 属性与 debug 日志记录 `prefix_break`（`model_switch|compaction|history_drop|observation_mask|session_refresh|unknown`）与命中率。新增指标 `polaris.llm.prefix_cache_hit_ratio{purpose, call_index_bucket}`。调用级 `[usage]` 行扩展为 `ctx=<当前估计>/<窗口> cache=<本回合命中率> turn=$x session=$y wall=Ns`。开发期诊断：`POLARIS_LLM_RAW_DUMP` 增加 `full` 模式写完整请求体（仅本机文件，默认关闭），配套 `tools/cache_diff.go` 报告相邻两次请求的首个分歧位置（消息下标、字节偏移、所属层）。

## 不做

| 方案 | 理由 |
|---|---|
| 常驻工具减到只剩 `tool_search` | 常驻工具缓存后约占 0.8% 费用；每次多一次往返的代价更高；Anthropic 官方建议也是保留 3–5 个高频常驻 |
| 仅 `run_code` 的 PTC 呈现 | DeepSeek 官方仍默认 native；日常单步读写改走程序反而更贵更慢；`code_act` 工具桥已覆盖多步数据流 |
| 依赖 DeepSeek Anthropic 端点的 `defer_loading`/`tool_addition` | 官方兼容文档未列出；`call_tool` 历史内激活对所有 provider 有效。可在后续以探测用例验证后作为增强 |
| 继续用 LLM 每回合重写用户画像 | 每回合打断整段缓存，输入质量差（只有 outcome 字符串） |

## 后果

- 预期：跨回合首调命中率从 ≈0–10% 升到 ≥85%（稳定层 + 会话层 + 已有历史）；回合内遮蔽断缓存只在长上下文时发生一次。按本机样本估算 kernel 费用下降 40–60%。真实效果以上线后 `llm_calls` 复核为准。
- 核心记忆在会话内的修改延迟到刷新点才进入会话层（工具结果已在历史中可见）。
- `call_tool` 调用的参数没有 provider 侧结构约束，依赖内核校验与错误回显 schema 重试。
- 历史保留更多，单次请求输入变大，但新增部分绝大多数按命中价计费。

## 引用代码

- `internal/gateway/server/chat/system_prompt.go`、`internal/memory/store/immutable_core_prompt.go`、`configs/prompts/system_prompt.md`
- `internal/agent/fsm/prompt_layers.go`、`prompt_respond.go`、`conversation.go`、`loop_transitions.go`
- `internal/agent/agent_lifecycle.go`（persona_refine 触发）、`internal/agent/context/persona_refiner.go`
- `internal/agent/agent_loop_masking.go`
- `internal/tool/catalog/lazy.go`、`composite.go`、`internal/tool/tool_search.go`
- `internal/action/codeact/bridge.go`、`internal/protocol/schema/013_chat.sql`

## 重新评估触发条件

- `prefix_cache_hit_ratio`（call_index ≥ 2）连续一周 < 80%。
- provider 价目中命中/未命中价差缩小到 < 5 倍（遮蔽与历史保留的成本模型需重算）。
- DeepSeek 官方文档列出 `defer_loading`/历史内工具变更 → 评估改用原生路径。
