# T7 阶段交付汇报：自动化页展示管线步骤、运行成本与数据产物

> 执行者：执行者组 C ｜ 仓库 core
> 依赖前置：T4、T6
> 提交哈希：`fb696d3` `feat(web): 自动化页展示管线步骤、运行成本与数据产物`

---

## 1. 任务交付概况

按照 `T7-frontend.md`、`_COMMON.md` 及 `docs/arch/M13-Interface-Scheduler.md §8.7 §8.9`（UI 规则，含 `inv_webui_*`）要求，完成 Web 前端与后端支撑改造：

1. **后端工作流列表与详情数据增强（Go）**：
   - `internal/gateway/server/sysadmin/workflowadmin/workflow.go`：
     - `workflow` 结构体新增 `LastRunSkipped bool`、`LastRunTokens int`、`LastRunCost float64` 序列化字段。
   - `internal/gateway/server/sysadmin/workflowadmin/workflow_handlers.go`：
     - `HandleListWorkflows`：
       - 使用窗口函数 `ROW_NUMBER() OVER (PARTITION BY workflow_id ORDER BY started_at DESC)` 高效批量查询各工作流最新一次运行记录（`lastRunMap`）。
       - 识别 `status == "skipped"` 状态，并调用 `WorkflowRepo.RunLLMUsage` 计算最近一次运行的 input/output token 汇总及费用折算，填入返回体。
     - `HandleGetWorkflow`：
       - 在详情接口中一并查询并填入该工作流最新运行的 `LastRunSkipped`、`LastRunTokens`、`LastRunCost`。

2. **前端国际化字典（i18n.js）**：
   - `web/src/js/i18n.js`：
     - 新增所有中英文映射字典项：
       - 来源徽标：`source_user`（用户 / User）、`source_agent`（智能体 / Agent）、`source_plugin`（插件 / Plugin）。
       - 状态徽标：`wf_status_skipped`（无新内容 / Skipped）、`wf_status_ok`（成功 / Success）、`wf_status_error`（失败 / Failed）、`wf_status_running`（运行中 / Running）。
       - 成本指标：`wf_last_run_tokens`（Token消耗 / Tokens）、`wf_last_run_cost`（费用估算 / Cost）。
       - 步骤分类：`wf_kind_agent`（智能体 / Agent）、`wf_kind_tool`（内置工具 / Tool）、`wf_kind_transform`（数据转换 / Transform）、`wf_kind_llm`（语言模型 / LLM）。
       - 步骤配置与错误：`wf_step_key`（步骤标识 / Step Key）、`wf_step_config_json`（配置参数 (JSON) / Config (JSON)）、`wf_step_config_json_invalid`（配置必须是合法的 JSON / Config must be valid JSON）。
       - 产物表格：`wf_run_step_data_title`（步骤数据产物 / Step Output Data）、`wf_table_title`（标题 / Title）、`wf_table_url`（链接 / URL）、`wf_table_published_at`（发布时间 / Published At）、`wf_table_no_data`（暂无结构化产物 / No structured items）、`wf_btn_view_data`（产物 / Output）。

3. **前端状态管理与逻辑层（workflow.js）**：
   - `web/src/js/store/workflow.js`：
     - `emptyStep` 支持 `step_key`、`kind`（默认为 `agent`）、`config_json_str`（格式化 JSON 字符串）、`dependencies_str` 等字段。
     - `openEdit` 支持解析 DAG / Pipeline 的步骤详情，非 agent 步骤自动将 `config` 格式化为多行缩进 JSON 字符串。
     - `save`：
       - 提交前对各步骤的 `config_json_str` 执行本地 `JSON.parse` 校验，非法时阻断保存并直接将错误反映在对应步骤的 `config_error` 字段中。
       - 接收后端 400 校验错误，将返回的 `field`（如 `steps[0].config.url`）精准定位到对应的 step，并在该步骤下方高亮展示错误信息。
     - 运行历史展示增强：
       - 实现 `formatDuration(ms)` 函数，友好展示毫秒与秒级耗时。
       - 实现 `openStepData(runId, stepKey)`：异步调取 `GET /v1/workflows/runs/{run_id}/data/{step_key}`。
       - 支持展示包含 `items`（最多 50 条表格展示 `title`、`url`、`published_at`）及原始 `output` 文本的多态结果查看模态框 `stepDataModal`。

4. **自动化界面视图更新（automation.html）**：
   - `web/src/pages/automation.html`：
     - **工作流列表卡片**：
       - 增加来源徽标（`user` / `agent` / `plugin`），不同徽标配色清晰区分。
       - 增加工作流类型徽标（`Chain` / `DAG`）。
       - 增加上次运行状态徽标（成功 / 失败 / 无新内容 [skipped] / 运行中）。
       - 增加上次运行 token 消耗及估算费用展示。
     - **工作流详情步骤列表**：
       - 步骤头部增加 `kind` 徽标或下拉框（`agent` / `tool` / `transform` / `llm`）。
       - 步骤标识 `step_key` 输入或只读展示。
       - JSON 配置编辑器：以等宽字体 textarea 展示并编辑 `config`，下方实时显示本地语法校验或后端返回的定位错误信息。
       - DAG 模式下显示步骤的前置依赖选择或输入框。
     - **运行历史卡片**：
       - 单次运行展示整体状态、耗时、总 token 与总成本。
       - 单步骤行增加「产物」操作按钮，点击弹出产物数据抽屉/弹窗。
     - **数据产物模态框**：
       - 顶部显示运行 ID、步骤名称及条目数量统计。
       - 若数据源包含 items 数组，渲染美观的数据条目表格（`title` 带超链接跳转、`url`、`published_at`）；同时提供原始 JSON/文本产物的展开查看。
     - **自动化任务（Prompt 型）列表**：
       - 同步增加来源徽标（`user` / `agent` / `plugin`），统一全站资产归属认知。
     - 规范合规：严格遵循 `inv_webui_11`（Tab 切换使用 `x-show`）及 `inv_webui_12`（Tab 容器使用 `tabs tabs-boxed`）。

---

## 2. 门控验证结果

- **前端构建**：`npm run build`（在 `web/` 目录）：
  - 成功打包生成 client production assets（0 error，耗时 ~380ms）。
- **后端测试**：`go test ./internal/gateway/server/... -race -count=1`：
  - 涵盖 `workflowadmin`、`sysadmin`、`chat`、`meshapi` 等全部包，测试全部通过（PASS）。
- **质量门控**：`make fmt && make lint && make docs-refs`：
  - 0 issues，代码格式化与规范检查全绿。
