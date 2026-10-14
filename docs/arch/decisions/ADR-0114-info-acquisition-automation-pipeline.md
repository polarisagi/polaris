# ADR-0114: 信息获取与自动化管线——确定性步骤、Web 数据面双通道、Agent 自建自动化

- **状态**: Accepted（2026-10-14 决策并全部落地；core 与官方插件仓库均已实现）
- **日期**: 2026-10-14
- **决策者**: MrLaoLiAI（设计：Claude）
- **相关模块**: M13（`internal/automation`、`sysadmin/cronadmin`、`sysadmin/workflowadmin`）/ M07（`internal/tool/builtin`）/ M08（StateGraphExecutor）/ M10（知识摄入）/ M13-bis（插件清单）/ 官方插件仓库 `polaris-plugins-official`

## 上下文

周期性信息采集此前只有一种执行方式：每次触发跑一轮完整 Headless Agent 会话（`automations` 与 `workflow_steps` 都是 prompt → RunTurn），导航、翻页、解析、去重全部消耗 token。

两份调研稿（爬虫/浏览器选型、自动化工作流架构；本地 `docs/research/`，不入库）的方向（确定性管线 0 token、LLM 只做最后提炼、Agent 自建自动化）成立，但以下事实经代码与外部资料核实后订正，是本决策的直接依据：

| # | 事实 | 证据 |
|---|---|---|
| F1 | Chrome 136 起 `--remote-debugging-port` 对默认用户目录失效；Chrome 144+ 的 `chrome://inspect` 远程调试开关对**每条新连接**弹「允许」，无「始终允许」。原插件读默认目录端口失败后回退为无登录的新 Chromium | Chrome 官方博客 remote-debugging-port；chrome-devtools-mcp issue #1794 |
| F2 | Agent 的 `cron_create` 写 `cron_jobs`（014），调度器只扫 `automations`（017）——Agent 建的定时任务从不执行 | 原 `cron_tools.go` 与 `cronadmin.cronTick` |
| F3 | Codex 插件定时任务只有 `key/name/prompt/schedule`，挂在市场远程元数据上，不是 DAG | `codex-rs/app-server-protocol/src/protocol/v2/plugin.rs` `ScheduledTaskSummary` |
| F4 | 官方插件清单位于 ADR-0103 已删除的 `.polaris-plugin/`，内核不读取 | `internal/extension/pluginspec/manifest.go` |
| F5 | 调研 B 提议的 `ALTER TABLE` 违反 DDL SSoT 规则 | `Test_inv_NoAlterTableInSchema` |

## 决策

**在现有 workflows + StateGraphExecutor + ToolRegistry 上增加确定性步骤类型，并把 Web 获取分为内核 HTTP 数据面与插件浏览器数据面两条通道；Agent 通过 `automation_*` 工具自建自动化，`cron_jobs` 全链路删除。**

1. **步骤类型**：`workflow_steps` 增 `step_key`、`kind ∈ {agent, tool, transform, llm}`、`config_json`（改 029 原文件）；步骤数据落 `workflow_run_data`，跨运行去重落 `workflow_seen_items`（仅成功收尾后写入）；`skip_if_empty` 使无新数据的运行不调 LLM、不投递。
   - `tool`：只经 `AgentToolExecutor.ExecuteWithTaint`；参数引用了上游数据（`{{steps.*}}`）即以 `TaintHigh` 调用。允许集 = 只读内置工具 ∪ `{web_fetch, feed_fetch}` ∪ 声明 `readOnlyHint` 的 MCP 工具（MCP 工具只看标注，不看 Capability 零值）∪ 写入白名单 `{knowledge_ingest}`；其余写工具必须声明 `approval:"hitl"`，**运行时**执行前经 HITL 网关审批，未声明或无审批通道一律拒绝（fail-closed，不依赖保存时校验）；`privileged` 与 `automation_*` 永远禁止。
   - `transform`：固定算子集（concat/dedup/filter/sort/limit/pick/rename/truncate），不引入表达式语言。
   - `llm`：单次 `Router.Infer`（default 池、默认关思考、输入字符与输出 token 硬上限、purpose=`workflow_llm_step`、`CtxTaskIDKey`=run_id 以便 `llm_calls` 按运行归账）；外部数据置于 user 消息定界块内。
   - `agent`：既有行为。
2. **纯逻辑包**：DSL 校验、模板渲染、算子与步骤执行器位于 `internal/automation/pipeline/`，依赖均为消费方定义接口，零 `net/http`、零 SQL；`workflowadmin` 只做分派与装配。
3. **HTTP 数据面（内核，Tier-0 可用）**：`fetch_url` 改名升级为 `web_fetch`（正文抽取 → Markdown、分页、条件请求缓存表 `web_fetch_cache`、按 host 礼貌间隔）；新增 `feed_fetch`（RSS/Atom/JSON Feed）；`web_search` 接通 `web_search_engine` 配置（duckduckgo/searxng/brave），统一返回 title/url/snippet。
4. **浏览器数据面（插件）**：复用用户日常 Chrome 的登录态，三后端自动选择——`extension`（自研 MV3 扩展「Polaris Browser Bridge」经 `chrome.debugger` 附着自建标签 + 插件进程内 CDP 中继，无弹窗、可无人值守，首选）→ `cdp`（Chrome ≥144 `chrome://inspect` 开关，每进程单条长连接、首次连接人工允许一次；未连接时管线采集返回 `needs_user_approval`，不主动发起）→ `managed`（系统 Chrome + 专用 profile，VPS/兜底）。所有后端只操作 Polaris 自建、非聚焦窗口内的标签（按唯一标记 URL 识别，不对用户标签开调试会话），不读 cookie 文件、不自动登录。站点配方（TOML，优先拦截页面 XHR JSON，CSS 兜底）+ 跨插件进程共享限速（SQLite）+ 风控信号熔断冷却；交互工具默认视口快照与正文抽取以省 token。采集能力并入 `browser_use`，不新建独立采集插件。
5. **Agent 自建**：`automation_list/save/run_now/delete` 取代 `cron_*`；`save` 支持 `kind=prompt`（→ automations）与 `kind=pipeline`（→ workflows），并支持 `dry_run` 试运行（不调 LLM、不执行写工具、不写去重表）；`source` 列区分 user/agent/plugin；后台/无头会话（`protocol.IsBackgroundWork`）调用写类 `automation_*` 在工具层直接拒绝；agent 来源启用数上限与最小触发间隔由阈值控制。
6. **插件伴生任务**：读 `extensions["ai.polarisagi"].scheduledTasks`，安装只注册为自动化模板，用户启用才生效；卸载删除模板并停用由其实例化的自动化。
7. **删除** `014_cron_jobs.sql`、`CronRepository`、`types.CronJobRow`、`cron_*` 工具；编号 014 永久留空。官方插件清单迁移到 `.codex-plugin/plugin.json`。

8. **插件分层（2026-10-14 复核追加，取代决策四中「采集能力并入 `browser_use`」）**：
   - `browser_use`：只做通用浏览器能力（打开、正文读取、快照、交互）与浏览器连接（BrowserHub、扩展桥接与中继），不含任何平台知识。
   - `social_poster` 更名为 `social`（MCP 服务器 `polaris-social`）：各平台的**读与写**同处一个插件——站点配方、登录检查、风控信号、按平台共享限速（读取与发帖计入同一账号的同一限速）。读工具 `social_search/social_hot/social_feed/social_read/social_platforms/social_selftest` 声明 `readOnlyHint`；发帖等写工具保持原名。
   - `knowledge_base` 升级为资料库：`kb_save` 把内容落成带 YAML 元信息头的 Markdown（默认 `~/Polaris/Vault`，Obsidian 兼容，按 URL 去重），另有 `kb_list/kb_search/kb_read/kb_tag/kb_move`，写入严格限定在资料库目录内。内核以 `ObsidianConnector` 同步资料库目录入 RAG，不另写入库逻辑。
   - 「提炼」仍只在管线 `llm` 步骤中进行（统一记账与预算），插件不调用模型。
   - 管线写入白名单改为阈值 `m13_interface.pipeline_write_allowlist`（默认 `knowledge_ingest`、`mcp__polaris-knowledge-base__kb_save`），取代代码内写死的单一名称。

阈值 SSoT：`docs/arch/spec/state.yaml §thresholds.m13_interface`（pipeline.* / automation.*）与 `§thresholds.m7_tool`（web_fetch.*）；代码只经 `config.CurrentThresholds()` 读取，不得写死。

## 后果

- **正向**: 周期采集的 LLM 调用收敛为每次运行至多一次提炼（无新数据则零次）；Agent 建的定时任务真正可执行；网页喂给模型的内容由整页文本变为正文 Markdown；VPS 部署仍可用 RSS/HTTP 通道；浏览器采集使用用户真实 Chrome 与持久登录态，限速跨插件生效。
- **负向**: 站点配方随平台改版需维护（以 beta 标注，`harvest_selftest` 报告字段完整率）；extension 后端需用户一次性加载扩展并粘贴配对令牌，cdp 后端每个插件进程首连需人工允许；复用日常账号意味着风控后果落在主账号，限速默认值保守；新增 3 个 Go 依赖（go-trafilatura / html-to-markdown v2 / gofeed）；写工具审批等待只受 HITL 网关自身超时约束。
- **反例守护**:
  - 提议「管线步骤里嵌入通用表达式语言/脚本」→ 拒绝，固定算子集即契约；需要新算子按本 ADR 追记增加。
  - 提议「写工具步骤审批只在保存时校验」或「无审批通道时放行」→ 拒绝，运行时 fail-closed。
  - 提议「失败后由 LLM 自动改写站点配方并直接生效」→ 拒绝（外部内容写入持久配置 = 注入持久化，HE-7）；只允许生成修复提议并经用户批准。
  - 提议「用 cdp 开关后端做无人值守采集并在断线后自动重连」→ 拒绝（每条连接需人工允许，无人时会堆积弹窗；无人值守用 extension 后端）。
  - 提议「读取/复制日常 Chrome 的 cookie 文件或 profile 目录以复用登录」或「操作用户已有标签页」→ 拒绝。
  - 提议「安装插件即自动启用其定时任务」→ 拒绝（ADR-0105 决策一：后台自动任务必须有开关）。
  - 提议「恢复 cron_jobs 或另建第三张调度表」→ 拒绝，调度 SSoT 为 automations + workflows。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 新建独立 DAG/工作流引擎（调研 B HDAW 原样落地） | 现有 StateGraphExecutor 已具备 DAG 并行、AND-Join、重试、断点续跑（ADR-0104）；只缺步骤类型 |
| `ALTER TABLE` 扩展 workflow_steps | 违反 DDL SSoT 规则，上线前直接改原文件 |
| 独立 `social_harvester` / `rss_reader` / `github_connector` 插件 | 多进程争用同一浏览器且限速无法全局；RSS 是纯 HTTP，应在内核；GitHub 已有官方 MCP Server |
| 管线中用 CEL / expr-lang 表达式过滤 | 新依赖 + 注入面 + 难以静态校验；固定算子已覆盖采集场景 |
| 只用托管专用 profile（用户在其中重新登录各平台） | 用户日常 Chrome 已登录全部平台；extension 后端可在不读凭证的前提下复用登录态 |
| nodriver 作为浏览器驱动 | 非 Playwright API（现有插件需重写）；基准优势主要来自使用系统 Chrome，patchright + 系统 Chrome 已获得 |

## 引用代码

- `internal/automation/pipeline/`（spec / validate / template / normalize / ops / runner）
- `internal/gateway/server/sysadmin/workflowadmin/workflow_step_worker.go`（步骤分派）、`pipeline_adapters.go`（工具/LLM/审批适配）、`pipeline_dryrun.go`
- `internal/tool/builtin/web_fetch/`、`feed_fetch/`、`web_search/`、`automation_tools.go`；`internal/extension/native/`（knowledge_ingest）
- `internal/protocol/schema/017_automations.sql`、`029_workflows.sql`、`049_web_fetch_cache.sql`
- 官方插件 `plugins/browser_use/src/browser_hub.py`、`bridge_relay.py`、`harvest/`、`recipes/`、`extension/`
- `docs/arch/M13-Interface-Scheduler.md §8.7`、`docs/arch/M07-Tool-Action-Layer.md`

## 重新评估触发条件

1. 真实运行 30 天后，管线中 `transform` 算子不足以表达的需求在 eval/工单中出现 ≥3 类 → 重新评估受限表达式语言。
2. 任一平台配方 selftest 字段完整率连续 2 周 <60% → 评估改用该平台 RSSHub 路由或官方 API，或下线该配方。
3. Chrome 提供「始终允许」类的远程调试授权，或 `chrome.debugger` 被限制/弃用 → 重新评估 extension 与 cdp 后端的优先级。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-14 | 初稿 |
| 2026-10-14 | 正文抽取选型：`github.com/markusmobius/go-trafilatura@v1.12.2` + `github.com/JohannesKaufmann/html-to-markdown/v2@v2.5.2`，fixtures 8/8 通过 |
| 2026-10-14 | 全部任务落地（core 32c73bb..38ebd22；plugins 3fb4c30..4ef4d53） |
| 2026-10-14 | 设计复核修订：写工具步骤运行时强制 HITL 审批且 fail-closed（3f7b39b、44f6aa6）；管线阈值改读配置（f06e50c）；浏览器自建页按标记 URL 识别、非聚焦窗口（plugins ae5f44d）。上下文补入调研订正表 F1~F5；决策五的拦截落点按代码订正为工具层 `IsBackgroundWork` 检查（原文写作 PolicyGate 拦截）；负向后果中「需在专用浏览器登录」随决策四改为扩展配对/cdp 首连允许 |
| 2026-10-14 | 追加决策八（插件分层）：采集从 `browser_use` 移入更名后的 `social`（读写同插件、共享限速），资料库插件落地 Markdown 并经 ObsidianConnector 入 RAG，写入白名单配置化。推翻决策四「采集并入 browser_use」的理由：读与写使用同一账号，限速与平台知识必须同处一处，否则一边采集一边发帖时频率叠加、风控特征需维护两份；browser_use 保持平台无关。实测补充：macOS 下非 Polaris 进程（如外部代理工具）可能无权读取 Chrome 数据目录中的 `DevToolsActivePort`，cdp 后端须由 Polaris 进程或用户终端运行；`chrome://inspect` 开关模式下 9222 端口不提供 HTTP 发现接口 |
