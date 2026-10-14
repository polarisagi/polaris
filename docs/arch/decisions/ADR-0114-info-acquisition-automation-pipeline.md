# ADR-0114: 信息获取与自动化管线——确定性步骤、Web 数据面双通道、Agent 自建自动化

- **状态**: Accepted（决策日 2026-10-14；落地进度见 `local_playground/upgrade/web-info-automation/PLAN.md §进度`，全部完成后在修订记录追记）
- **日期**: 2026-10-14
- **决策者**: MrLaoLiAI（设计：Claude）
- **相关模块**: M13（`internal/automation`、`sysadmin/cronadmin`、`sysadmin/workflowadmin`）/ M07（`internal/tool/builtin`）/ M08（StateGraphExecutor）/ M10（知识摄入）/ M13-bis（插件清单）/ 官方插件仓库

## 上下文

周期性信息采集此前只有一种执行方式：每次触发跑一轮完整 Headless Agent 会话（`automations` 与 `workflow_steps` 都是 prompt → RunTurn），导航、翻页、解析、去重全部消耗 token。Agent 侧的 `cron_create` 写入 `cron_jobs`（014），但调度器只扫 `automations`（017），Agent 创建的定时任务从不执行。网页读取（`fetch_url`）为正则去标签、无正文抽取；`web_search` 只回摘要无 URL；浏览器插件依赖读取 Chrome 默认目录的 `DevToolsActivePort`，自 Chrome 136 起对默认 profile 无效，实际退化为无登录的新 Chromium。

## 决策

**在现有 workflows + StateGraphExecutor + ToolRegistry 上增加确定性步骤类型，并把 Web 获取分为内核 HTTP 数据面与插件浏览器数据面两条通道；Agent 通过 `automation_*` 工具自建自动化，`cron_jobs` 全链路删除。**

1. **步骤类型**：`workflow_steps` 增 `step_key`、`kind ∈ {agent, tool, transform, llm}`、`config_json`（改 029 原文件）。`tool` 只经 `AgentToolExecutor.ExecuteWithTaint`；`transform` 为固定算子集（concat/dedup/filter/sort/limit/pick/rename/truncate），不引入表达式语言；`llm` 为单次 `Provider.Infer`（default 池、默认关思考、输入字符与输出 token 硬上限、purpose=`workflow_llm_step`）；`agent` 为既有行为。步骤数据落 `workflow_run_data`，跨运行去重落 `workflow_seen_items`。`skip_if_empty` 使无新数据的运行不调 LLM、不投递。
2. **纯逻辑包**：DSL 校验、模板渲染、算子与步骤执行器位于 `internal/automation/pipeline/`，依赖均为消费方定义接口，零 `net/http`、零 SQL；`workflowadmin` 只做分派与装配。
3. **HTTP 数据面（内核，Tier-0 可用）**：`fetch_url` 改名升级为 `web_fetch`（正文抽取 → Markdown、分页、条件请求缓存表 `web_fetch_cache`、按 host 礼貌间隔）；新增 `feed_fetch`（RSS/Atom/JSON Feed）；`web_search` 接通 `web_search_engine` 配置（duckduckgo/searxng/brave），统一返回 title/url/snippet。
4. **浏览器数据面（插件）**：复用用户日常 Chrome 的登录态，三后端自动选择——`extension`（自研 MV3 扩展「Polaris Browser Bridge」经 `chrome.debugger` 附着自建标签 + 插件进程内 CDP 中继，无弹窗、可无人值守，首选）→ `cdp`（Chrome ≥144 `chrome://inspect` 开关，每进程单条长连接、首次连接人工允许一次；未连接时管线采集返回 `needs_user_approval` 不主动发起）→ `managed`（系统 Chrome + 专用 profile，VPS/兜底）。所有后端只操作 Polaris 自建窗口内的标签，不读 cookie 文件、不自动登录。站点配方（TOML，优先拦截页面 XHR JSON，CSS 兜底）+ 跨插件进程共享限速（SQLite）+ 风控信号熔断冷却；交互工具默认视口快照与正文抽取以省 token。采集能力并入 `browser_use`，不新建独立采集插件。
5. **Agent 自建**：`automation_list/save/run_now/delete` 取代 `cron_*`；`save` 支持 `kind=prompt`（→ automations）与 `kind=pipeline`（→ workflows）；`source` 列区分 user/agent/plugin；无头来源（automation/workflow/插件 channel 会话）禁止调用写类 `automation_*`；agent 来源启用数上限与最小触发间隔由阈值控制。
6. **插件伴生任务**：读 `extensions["ai.polarisagi"].scheduledTasks`，安装只注册为自动化模板，用户启用才生效；卸载删除模板并停用由其实例化的自动化。
7. **删除** `014_cron_jobs.sql`、`CronRepository`、`types.CronJobRow`、`cron_*` 工具；编号 014 永久留空。官方插件清单由已废弃的 `.polaris-plugin/` 迁移到 `.codex-plugin/plugin.json`。

阈值 SSoT：`docs/arch/spec/state.yaml §thresholds.m13_interface`（pipeline.* / automation.*）与 `§thresholds.m7_tool`（web_fetch.*）。

## 后果

- **正向**: 周期采集的 LLM 调用收敛为每次运行至多一次提炼（无新数据则零次）；Agent 建的定时任务真正可执行；网页喂给模型的内容由整页文本变为正文 Markdown；VPS 部署仍可用 RSS/HTTP 通道；浏览器采集使用真实 Chrome 与持久登录态，限速跨插件生效。
- **负向**: 站点配方随平台改版需维护（以 beta 标注，selftest 报告字段完整率）；用户需在专用浏览器中登录各平台一次；新增 3 个 Go 依赖（正文抽取 / HTML→Markdown / feed 解析）。
- **反例守护**:
  - 提议「管线步骤里嵌入通用表达式语言/脚本」→ 拒绝，固定算子集即契约；需要新算子按本 ADR 追记增加。
  - 提议「失败后由 LLM 自动改写站点配方并直接生效」→ 拒绝（外部内容写入持久配置 = 注入持久化，HE-7）；只允许生成修复提议并经用户批准。
  - 提议「用 cdp 开关后端做无人值守采集并在断线后自动重连」→ 拒绝（每条连接需人工允许，无人时会堆积弹窗；无人值守用 extension 后端）。
  - 提议「读取/复制日常 Chrome 的 cookie 文件或 profile 目录以复用登录」→ 拒绝（凭证外泄面 + 加密钥绑定，ADR-0114 只允许经浏览器本身使用登录态）。
  - 提议「操作用户已有标签页」→ 拒绝，只操作 Polaris 自建窗口内标签。
  - 提议「安装插件即自动启用其定时任务」→ 拒绝（ADR-0105 决策一：后台自动任务必须有开关）。
  - 提议「恢复 cron_jobs 或另建第三张调度表」→ 拒绝，调度 SSoT 为 automations + workflows。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 新建独立 DAG/工作流引擎（调研 B HDAW 原样落地） | 现有 StateGraphExecutor 已具备 DAG 并行、AND-Join、重试、断点续跑（ADR-0104）；只缺步骤类型 |
| `ALTER TABLE` 扩展 workflow_steps | 违反 DDL SSoT 规则（`Test_inv_NoAlterTableInSchema`），上线前直接改原文件 |
| 独立 `social_harvester` / `rss_reader` / `github_connector` 插件 | 多进程争用同一 Chrome profile 锁且限速无法全局；RSS 是纯 HTTP，应在内核；GitHub 已有官方 MCP Server |
| 管线中用 CEL / expr-lang 表达式过滤 | 新依赖 + 注入面 + 难以静态校验；固定算子已覆盖采集场景 |
| nodriver 作为浏览器驱动 | 非 Playwright API（现有插件需重写）；基准优势主要来自使用系统 Chrome，patchright + `channel=chrome` 已获得 |

## 引用代码

- `internal/gateway/server/sysadmin/workflowadmin/workflow_step_worker.go`（步骤分派点）
- `internal/automation/pipeline/`（新增）
- `internal/tool/builtin/web_fetch/`、`feed_fetch/`、`web_search/`、`automation_tools*.go`、`knowledge_ingest/`
- `internal/protocol/schema/017_automations.sql`、`029_workflows.sql`、`049_web_fetch_cache.sql`
- 官方插件 `plugins/browser_use/src/browser_hub.py`、`recipes/`
- `docs/arch/M13-Interface-Scheduler.md §8.7`、`docs/arch/M07-Tool-Action-Layer.md`

## 重新评估触发条件

1. 真实运行 30 天后，管线中 `transform` 算子不足以表达的需求在 eval/工单中出现 ≥3 类 → 重新评估受限表达式语言。
2. 任一平台配方 selftest 字段完整率连续 2 周 <60% → 评估改用该平台 RSSHub 路由或官方 API，或下线该配方。
3. Chrome 提供「始终允许」类的远程调试授权，或 `chrome.debugger` 被 Chrome 限制/弃用 → 重新评估 extension 与 cdp 后端的优先级。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-14 | 初稿（设计依据：`local_playground/upgrade/web-info-automation/00-DESIGN.md`；正文抽取库选型数据待 T1 追记） |
| 2026-10-14 | 正文抽取选型：`github.com/markusmobius/go-trafilatura@v1.12.2` + `github.com/JohannesKaufmann/html-to-markdown/v2@v2.5.2`，fixtures 结果 8/8 全通 |
| 2026-10-14 | 全部任务落地：core 交付 T0~T7, T10, T11 (32c73bb..main)；plugins 交付 T8, T8b, T9 (3fb4c30..2d0cfc3) |
