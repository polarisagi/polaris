# ADR-0119: 首轮输入 token 瘦身——不依赖 Embedder 的工具懒加载

- **状态**: Accepted
- **日期**: 2026-10-20
- **决策者**: MrLaoLiAI（设计：Claude；实现：Sonnet）
- **相关模块**: M13-bis（工具目录与 tool_search）/ M04（提示组装）/ internal/tool/catalog / internal/tool / configs/prompts
- **修订**: ADR-0105 决策三（tools 数组只追加）——核心集改为静态且不依赖 Embedder；元工具由 `search_tools` 统一为已注册的内置工具 `tool_search`

## 上下文

用户实测一个简单问题的首轮输入约 1.39 万 token，其中 56 个工具的完整定义约占 78%。懒加载早已存在，但开启条件是"工具数 > 40 **且配了 Embedder**"，用户机器没有 Embedder，于是每轮全量下发。另有两处冗余：七份运营指令（约 8KB，含重复表述）每轮注入；电脑操控策略在未开启任何电脑操控能力时也会注入。

## 决策

### 决策一：懒加载与 Embedder 解耦

条件改为"工具数 > 阈值，且目录里有 `tool_search`"（没有它就无从激活，此时宁可全量下发）。常驻核心集是静态的 14 个：`read_file list_dir glob grep write_file str_replace_editor bash run_command web_search web_fetch memory_search todo_write spawn_agents tool_search`（回合内另有 `code_act`，合计 ≤15）。选取依据是日常使用频率：文件读写搜索、命令、联网、记忆检索、任务清单、并发子智能体，加上激活其余工具的入口。

### 决策二：未加载工具以一行索引进入系统提示

`CompositeCatalog.ToolsHint` 渲染"已加载核心工具名 + 未加载工具索引"：内置工具按类别分组，每个一行"名称 用途"（用途取描述首句、≤22 字）；MCP 按服务器只列名称；技能只列名称。索引列出**全部**非核心工具，与本会话激活无关，所以字节只随工具集（装卸 MCP/技能）变化，不破坏 L0/L1 前缀缓存。

### 决策三：`tool_search` 的关键词/BM25 检索是基线，向量是增强

BM25 文档 = 工具名（3 倍权重）+ 描述 + 检索别名；分词对汉字取单字加相邻二元组，不依赖词典。**检索别名**（`catalog.SearchAliases`）是中英双语关键词与类别名，只喂检索、不进提示词、不占 token——因为不少工具描述只有英文，而用户常用中文（"语音朗读"检索不到 "Convert text to speech"），可达性评测第一次跑就暴露了这点。Embedder 存在时叠加语义分，两路取大者；点名匹配置顶。核心工具不进结果。激活后的工具在**下一轮**出现在 tools 数组尾部（只追加、同一工具重复激活位置不变，单会话上限 64、淘汰最早项），并在会话内一直保留。

### 决策四：描述与参数文档压缩

1. 内置 schema 加载时去掉顶层 `$schema/title/description/$id`（对模型无信息量）。
2. 体积最大的一批工具（`automation_save core_memory_edit grep search_extension spawn_agents web_fetch todo_write switch_model git_diff` 等）重写描述与参数说明：先说用途，去掉重复与示例赘述，约定"描述首句即用途"（索引依赖此约定）。
3. 七份运营指令合并去重为 `operational/directives.md`（约 8.0KB → 2.6KB），规则语义不变。
4. 电脑操控策略仅在开启了任一电脑操控能力（`computer_any_app_enabled` / `computer_chrome_enabled`）时注入。

### 决策五：门控与评测

- `internal/agent/token_budget_test.go` 用生产的组装函数拼出首轮请求，估算口径同 REPORT-usage-steps-leftovers C5（实测偏低约 3%），**简单问题首轮输入 ≤ 7000 token** 且首轮 tools ≤ 核心集上限，超限即失败。
- `internal/agent/tool_reach_eval_test.go`：对 adr0114 与 adr0119 两组 EvalCase 的期望工具，比较改前（全量下发）与改后（核心直达，或经真实 `tool_search` 激活）的可达率，**下降 > 2 个百分点即失败**。它是确定性代理，度量"检索通路不丢工具"，不是模型选择能力；真实模型下的选择准确率需上线后用 `llm_calls` 复核。
- 新增用例 `adr0119_tool_search_cases.json`（12 条"非核心任务：先 tool_search 再调用"），由 harness 的 `TrajectoryJudge` 判定。

## 备选方案

| 方案 | 驳回理由 |
|------|---------|
| 要求用户配置 Embedder | 用户机器无 Embedder，且关键词检索对"按名称/用途找工具"足够 |
| 提高阈值让 56 个工具不触发懒加载 | 直接放弃省 token 的目标 |
| 核心集按会话动态选择 | 破坏 tools 数组字节稳定，前缀缓存失效（ADR-0105） |
| 系统提示里只放工具名不放用途 | 模型无法判断该激活哪个，只能盲搜 |
| 删除低频工具 | 能力损失；懒加载已能无损降本 |

## 后果

- 简单问题首轮输入估算 14340 → 约 4700 token（见 REPORT）。
- 非核心工具多一次 `tool_search` 往返（一次模型调用）；常用工具应在评测显示可达率或延迟退化时并入核心集。
- 工具描述首句必须是用途；新增非核心工具时应补 `SearchAliases`（中文关键词）。

## 引用代码

- `internal/tool/catalog/composite.go`、`lazy.go`（核心集、`ToolsHint`、`SearchAliases`）
- `internal/tool/tool_search.go`、`tool_search_bm25.go`
- `internal/gateway/server/chat/system_prompt.go`、`prompt_assembly_service.go`（`ToolIndexer`）
- `configs/prompts/operational/directives.md`、`internal/agent/fsm/state_machine_prompts.go`
- `internal/agent/token_budget_test.go`、`tool_reach_eval_test.go`、`internal/eval/harness/testdata/adr0119_tool_search_cases.json`
