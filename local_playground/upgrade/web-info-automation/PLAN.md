# PLAN — 信息获取 × 自动化管线（W8 契约文件）

> 设计方：Claude（只设计与复核，不写代码）｜实现方：Sonnet / Gemini 等｜创建：2026-10-14
> 设计依据：`00-DESIGN.md`；ADR 草案：`ADR-0114-draft.md`；公共规则：`_COMMON.md`（每个任务开工前必读）；复核清单：`REVIEW-CHECKLIST.md`
> 两个仓库：内核 `/Users/mrlaoliai/PolarisAGI/polaris`（下称 core）｜插件 `/Users/mrlaoliai/PolarisAGI/polaris-plugins-official`（下称 plugins）

## 目标

1. Agent 能把周期性信息需求编译为「确定性管线 + 一次 LLM 提炼」并注册运行；数据步骤 0 token。
2. Web 获取分两条通道：core 内 HTTP 数据面（web_fetch / feed_fetch / web_search，VPS 可用）；plugins 内浏览器数据面（真实 Chrome 专用 profile + 站点配方 + 全局限速）。
3. 修复：Agent `cron_create` 建的任务从不执行（F4）；官方插件清单不被识别（F5）；浏览器附着在 Chrome 136+ 失效（F1）。
4. 浏览器复用用户日常 Chrome（155，已登录各平台）的登录态：扩展桥接免弹窗可无人值守，cdp 开关附着即时可用，托管 profile 兜底；只在 Polaris 自建窗口内操作。

## 硬约束（违反即退回）

- CLAUDE.md 全部 [强制] 条款；中文注释只写「为什么」；`pkg/apperr`；`concurrent.SafeGo`；接口在调用方定义；工具执行只经 `ExecuteWithTaint`/`ExecuteTool`，不得旁路。
- DDL：只改原建表文件（上线前）；新表放新编号文件（`ls internal/protocol/schema/*.sql` 取最大编号 +1）；禁止 ALTER。
- 规范性先行：T0（ADR / state.yaml / M 文档）合入后，其余任务才能开工。
- 不做设计外的功能、不顺手重构；发现规范与代码冲突 → 记入汇报，不改。
- 每个任务结束提交**交付证据**（命令 + 关键输出），见 `_COMMON.md §汇报格式`。
- 提交署名 `MrLaoLiAI <polarisagi.online@gmail.com>`，**禁止任何 AI Co-authored-by**；提交信息 `<type>(<scope>): <中文述>`；所有代码最终在 main。
- `git push`、删除开发库 —— 须用户确认。

## 任务与文件所有权

| 任务 | 仓库 | 执行者组 | 依赖 | 独占文件/目录 |
|---|---|---|---|---|
| T0 文档先行 | core | A | — | `docs/arch/decisions/ADR-0114-*.md`、`decisions/README.md`、`docs/arch/spec/state.yaml`(新节)、`M13-Interface-Scheduler.md §8.7`、`M07-Tool-Action-Layer.md`(工具清单段)、`docs/specs/CHANGELOG.md`、`CLAUDE.md`(schema 计数行) |
| T1 web_fetch | core | A | T0 | `internal/tool/builtin/fetch_url/`→`web_fetch/`、`internal/tool/builtin/builtin_tools.go`、`internal/tool/catalog/tool_restriction.go`、新 DDL `0NN_web_fetch_cache.sql`、`internal/store/repo/repo_webcache.go`、`testdata/webfetch/` |
| T2 feed_fetch + web_search | core | A | T1 | `internal/tool/builtin/feed_fetch/`、`internal/tool/builtin/web_search/`、`configs/defaults.toml [agent.skill]`、`internal/config/config_types.go`(web_search 字段) |
| T3 automation_* 替换 cron_* | core | A | T2 | `internal/tool/builtin/automation_tools*.go`、`automation_*/` 元数据目录、删除 `cron_*`、`014_cron_jobs.sql`、`repo_cron.go`、`types.CronJobRow`、`protocol.CronRepository`、`sysadmin/doctor.go`(cron 计数)、`017_automations.sql`(source 列)、`internal/security/policy/gate_builtin_rules.go` |
| T5 knowledge_ingest | core | A | T3 | `internal/tool/builtin/knowledge_ingest/`、装配点 |
| T4 管线引擎 | core | B | T0 | `internal/automation/pipeline/`(新)、`029_workflows.sql`、`internal/protocol/repo/repo_workflow.go`、`internal/store/repo/repo_workflow.go`、`sysadmin/workflowadmin/*`、指标定义文件（按现有 metrics 包约定） |
| T6 pipeline 保存 + dry-run | core | A | T3,T4,T5 | `automation_tools*.go`（扩展 kind=pipeline）、`workflowadmin` 对外导出的保存/试运行入口、`internal/extension/mcp` 中 MCP 工具注册处（ToolAnnotations 解析） |
| T7 前端 | core | C | T4,T6 | `web/src/pages/automation.html`、`web/src/js/store/workflow.js`、`web/src/js/i18n.js` |
| T8 browser_use v2 + 清单迁移 | plugins | D | T0 | `plugins/browser_use/**`、所有插件 `.polaris-plugin/`→`.codex-plugin/`、`plugins/social_poster/src/utils/browser*.py`、`check.sh`、`CLAUDE.md`、`.agents/plugins/marketplace.json` |
| T8b 浏览器桥接扩展 + CDP 中继 | plugins | D | T8 | `plugins/browser_use/extension/**`、`plugins/browser_use/src/bridge_relay.py`、`bridge_const.py`、`browser_hub.py`（ExtensionBackend，并同步副本到 social_poster） |
| T9 站点配方 | plugins | D | T8（T8b 建议先完成） | `plugins/browser_use/recipes/*.toml`、`plugins/browser_use/tests/fixtures/**`、`plugins/browser_use/skills/**` |
| T10 插件伴生任务模板 | core | A | T6 | `internal/extension/pluginspec/`(ai.polarisagi.scheduledTasks)、`lifecycle/plugin_installer.go`、`cronadmin/cron_templates_handlers.go` |
| T11 eval + 收尾门控 | core | 收尾者 | 全部 | eval 用例目录、共享生成物（`make gen-threshold-examples`/`docs-sync`/`docs-gen`/`generate-manifest`） |

并行规则：A、B、D 三组可并行；同组串行。`builtin_tools.go` 仅 A 组改。共享生成命令只由 T11 收尾者运行（W8.3）。

**A/B 两组共享、只追加不改他人行的文件**（先 `git pull --rebase` 再改，冲突手工合并）：`internal/config/thresholds.go`（A 改 `M7ToolThresholds` 与 `M13Interface` 的 automation.*，B 改 `M13Interface` 的 pipeline.*）、`internal/observability/metrics/instruments.go`/`instruments_init.go`/`record.go`、`internal/protocol/interfaces_automation.go`（A 加 DTO）、`internal/gateway/server/server_core.go`（A 加 `AutomationFacade()`，B 加 `SetPipelineDeps`）、`cmd/polaris/boot_server.go`（同上两处装配）、`sysadmin/cronadmin/cron_runner.go`（A 在 T3 改服务化，B 在 T4 抽 `DeliverResult`——**B 必须在 T3 合入后再做 T4 §5.3**）。

## 执行顺序

```
T0 ─┬─ A: T1 → T2 → T3 → T5 ─┐
    ├─ B: T4 ────────────────┼─ A: T6 → T10 ─┐
    └─ D: T8 → T8b → T9      └─ C: T7 ───────┴─ T11（收尾者）→ 设计方复核
```

## §进度

> 实现方每完成一个任务在此打勾并附提交哈希；设计方复核后补「✔复核」。中断后从这里续接。

- [x] T0 文档先行 — 32c73bb
- [x] T1 web_fetch — f326485, 226007d, bc5aab6, c78fdb2
- [x] T2 feed_fetch + web_search — 771928b, 0f8f7d8
- [x] T3 automation_* 替换 cron_* — f3e6f4c, e7904fe, 04837ad, dd17169
- [x] T5 knowledge_ingest — 64364bf
- [x] T6 pipeline 保存 + dry-run — 54a8def, b0bef73, 2ab5a3f
- [x] T7 前端 — fb696d3
- [x] T8 browser_use v2 + 清单迁移 — 3fb4c30, c182a63, bb0a817, 0ec6701, 4eca192, c19398b, 1daebea
- [ ] T8b 浏览器桥接扩展 + CDP 中继 —
- [ ] T9 站点配方 —
- [x] T10 插件伴生任务模板 — 0b65c90
- [ ] T11 eval + 收尾门控 —
- [ ] 设计方终审 —
