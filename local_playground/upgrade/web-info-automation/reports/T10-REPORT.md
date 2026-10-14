# T10 阶段交付汇报：插件伴生任务 → 自动化模板（安装 ≠ 启用）

> 执行者：执行者组 A ｜ 仓库 core
> 依赖前置：T6
> 提交哈希：`0b65c90` `feat(extension): 交付插件伴生任务解析、模板化注册与占位解析`

---

## 1. 任务交付概况

按照 `T10-plugin-scheduled-tasks.md`、`00-DESIGN.md §1 F3` 以及 ADR-0103（决策二/决策三）规范，完成插件伴生任务清单解析、自动化模板动态暴露、MCP 占位符实例化解析与卸载停用全流程：

1. **清单模型与严格对象解析（`internal/extension/pluginspec`）**：
   - 新增 `component_tasks.go`：
     - 定义 `ScheduledTask` 模型（`Key`、`Name`、`Description`、`Kind`、`Schedule`、`Prompt`、`Pipeline`）。
     - 实现 `loadScheduledTasks(docs, ds)`：解析 `extensions["ai.polarisagi"].scheduledTasks[]`。
     - 严格对象校验：使用 `json.NewDecoder.DisallowUnknownFields()`。若存在未知键或字段非法（如 `key` 不匹配 `^[a-z0-9_]{1,40}$`、`kind` 非 `prompt|pipeline`、`schedule` 非 5 字段 cron 等），仅将该项失败并记入对应规则诊断（`RuleScheduledTasksShape`、`RuleScheduledTasksKey`、`RuleScheduledTasksCron`），不影响插件其余组件的加载。
     - 写入归一化内部模型 `Plugin.ScheduledTasks`，并在插件为空判断中一并纳入。
   - 实现 MCP 工具占位符解析：
     - `ResolveMCPPlaceholders(input []byte, resolveServer func(server string) string) []byte`：支持提取 `${MCP:<server>:<tool>}` 并替换为实际的运行时工具名。

2. **存储方式评估与安装注册（`lifecycle/plugin_installer.go`）**：
   - **存储方式选择与理由说明**：
     - 现有模板发现机制主要依赖静态内嵌的 `automation_sources.yaml`，无法动态感知运行时热插拔安装的插件；而扩展体系已具备统一的 `ExtensionRepository` 持久化插件归一化清单（`plugins.manifest`，State-in-DB）。
     - 因此采取**双向自包含存储方案**：
       ① 插件安装时，清单快照（含 `ScheduledTasks`）自动随 `types.PluginRow.Manifest` 落库持久化，作为状态权威源；
       ② 同时在插件安装目录下生成 `automation_templates.yaml` 本地模板文件，保持插件目录的文件自包含性；
       ③ 严格遵守规则：安装时不创建 `automations`/`workflows` 运行实例行（安装 ≠ 启用）。

3. **模板发现接口与来源聚合（`cronadmin/cron_templates_handlers.go`）**：
   - `CronAdmin` 结构体注入 `ExtRepo protocol.ExtensionRepository`，在 `NewSysAdminHandler` 装配点建立连接。
   - `HandleListAutomationTemplates`：
     - 调用 `loadPluginTemplates`，遍历所有已启用的插件，将 `ScheduledTasks` 聚合为 `automationTemplate` 列表，标明 `source: "plugin:<plugin_id>"` 及 `tags: ["plugin", <plugin_name>]`。
     - `automationTemplate` 扩展支持 `Kind`（`prompt` | `pipeline`）与 `Pipeline`（JSON spec）。
     - 支持通过 `?source=plugin:<id>` 或 `?tag=plugin` 进行精准过滤。

4. **从模板创建与 MCP 占位符实例化解析（`workflowadmin/workflow_handlers.go`）**：
   - `handlePipelineCreate` 与 `HandleCreateWorkflow` 支持提取并透传请求中的 `source`（如 `plugin:pl_xxx`）。
   - `SavePipeline` 与 `DryRunPipeline`：
     - 在执行 `validateSpec` 校验之前，调用 `resolvePipelineMCPPlaceholders` 解析 `${MCP:<server>:<tool>}` 占位符。
     - 通过查询 `mcp_servers` 表及作用域服务名（如 `<plugin_name>-<server>`），将占位符替换为真实的工具名（如 `mcp__crawler-browser__harvest`）。
     - 解析后的工具名称经 `pipeline.Validate` 走标准 PolicyGate 与工具元数据检查，若工具未就绪则友好报错阻断。

5. **卸载时清理与用户数据保留（`repo_extension_mcp.go` & `plugin_installer.go`）**：
   - `UninstallCleanup`：
     - 在事务内执行 `UPDATE automations SET enabled=0, last_run_error='来源插件已卸载' WHERE source=?`（`source = "plugin:" + id`）。
     - 同样执行 `UPDATE workflows SET enabled=0, last_run_error='来源插件已卸载' WHERE source=?`。
     - 仅停用任务并标明卸载原因，保留全部用户历史数据（prompt、steps、配置等）。
   - `PluginInstaller.Uninstall`：
     - 清理插件目录下的 `automation_templates.yaml`。
     - 删除插件清单行后，模板接口自动不再返回该插件模板。

---

## 2. 门控验证结果

- **单元测试**：
  - `internal/extension/pluginspec`：
    - `TestScheduledTasks_Valid`：验证 prompt 与 pipeline 任务解析（PASS）。
    - `TestScheduledTasks_StrictObjectAndValidation`：验证未知字段被拦截并记入诊断，合法项保留（PASS）。
    - `TestResolveMCPPlaceholders`：验证占位符正确替换及未匹配占位符保留（PASS）。
- **端到端集成测试**：
  - `internal/extension/lifecycle/plugin_scheduled_tasks_test.go`：
    - `TestPlugin_ScheduledTasksLifecycle`：完整覆盖安装 → 模板可见 → 从模板创建实例 → 卸载 → 模板消失且实例停用保留数据（PASS）。
- **模块回归测试**：
  - `go test ./internal/extension/... ./internal/gateway/server/sysadmin/cronadmin/... ./internal/gateway/server/sysadmin/workflowadmin/... -race -count=1`：全部 10 个包 PASS。
- **全局代码质量门控**：
  - `make fmt && make lint && make docs-refs`：
    - 全部静态规则、19 个专用 Lint 门控、`golangci-lint`（0 issues）、文档引用全部 PASS。
