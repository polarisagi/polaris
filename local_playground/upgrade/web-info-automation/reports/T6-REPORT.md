# T6 阶段交付汇报：automation_save(kind=pipeline) + dry-run + MCP 只读标注

> 执行者：执行者组 A ｜ 仓库 core
> 依赖前置：T3、T4、T5
> 提交清单：
> - 提交 ①：`54a8def` `feat(mcp): 解析 MCP ToolAnnotations 并为管线只读判定提供提示支持`
> - 提交 ②：`b0bef73` `feat(workflow): 交付管线同步试运行 DryRun 与 AutomationFacade 双引擎扩展`
> - 提交 ③：`2ab5a3f` `feat(tool): automation_save 支持 pipeline 与 dry-run 同步试运行`

---

## 1. 任务交付概况

本阶段按照 `T6-pipeline-save-dryrun.md` 及 `ADR-0114` 设计规范，交付了 MCP 工具注解解析、只读提示、workflowadmin 同步试运行（DryRun）、AutomationFacade 双引擎整合以及内置工具层 `automation_*` 对 `pipeline` 与 `dry-run` 的全面支持：

1. **MCP ToolAnnotations 与管线只读提示（Commit ①）**：
   - `internal/protocol/mcp.go`：新增 `MCPToolAnnotations` 结构体，支持 `title`、`readOnlyHint`、`destructiveHint`、`idempotentHint`、`openWorldHint`；`MCPTool` 增加 `Annotations` 解析字段。
   - `pkg/types/models_tool.go`：`Tool` 结构体增加 `ReadOnlyHint bool` 字段，作为 MCP 来源工具的提示性只读标记。
   - `internal/extension/mcp/mcp_manager_tools.go`：`registryTool` 在注册 MCP 工具时计算 `ReadOnlyHint = (readOnlyHint == true && destructiveHint != true)`；`toolEqual` 对比 `Annotations`。
   - `internal/gateway/server/sysadmin/workflowadmin/pipeline_meta_adapter.go`：更新 `toolMetaAdapter.ToolMeta`，对 MCP 来源工具仅以 `t.ReadOnlyHint` 作为只读判断，避免 Capability 零值误判。

2. **workflowadmin 同步试运行与 AutomationFacade 统一编排（Commit ②）**：
   - `internal/protocol/interfaces_automation.go`：定义试运行 DTO `PipelineDryRunStep`、`PipelineDryRunResult`、`PipelineSaveResult`、`FieldError`。
   - `internal/gateway/server/sysadmin/workflowadmin/pipeline_dryrun.go`：
     - 实现 `DryRunPipeline(ctx, spec)` 与 `DryRunWorkflow(ctx, id)`，120 秒整体超时。
     - 基于拓扑排序（Kahn 算法）顺序执行各步骤，采用只读 DedupState（查库去重，不写 seen 表）与内存 RunDataStore。
     - 工具步骤参数自动夹紧（`limit` / `max_items` / `max_results` 取 `min(val, 5)`）；`agent` 步骤标记 `status: "not_run"` 且不执行。
     - 输出截断保护：单步 preview 最多 3 条（每条 ≤120 rune），整体序列化输出强制 ≤2000 rune。
   - `internal/gateway/server/sysadmin/workflowadmin/workflow_facade.go`：
     - 扩展 `WorkflowAdmin`，提供 `ListPipelineSummaries`（聚合最近 7 天 LLM Token 消耗）、`CountEnabledBySource`、`DeleteWorkflow`、`TriggerWorkflow`。
   - `internal/gateway/server/sysadmin/automation_facade.go`：
     - 统一挂载 `Workflow *workflowadmin.WorkflowAdmin`。
     - `List`：聚合 Cron 与 Pipeline，补充 `kind: "pipeline"` 及 7 天 token 消耗。
     - `SavePipeline`、`DryRunSpec`、`DryRunByID`：委托给 `Workflow`。
     - `Delete`、`RunNow`：根据 ID 前缀智能分派（`wf_` 路由至工作流，`auto_` 路由至 Cron）。
     - `CountEnabledBySource`：合并 Cron 与工作流双表统计。
   - `internal/gateway/server/server_core.go`：`Server.AutomationFacade()` 正确绑定 `s.sysadminHandler.Workflow`。

3. **内置工具层 pipeline、dry-run 与 Bridge 交付（Commit ③）**：
   - `internal/tool/builtin/automation_tools.go`：
     - `AutomationService` 接口扩充 `SavePipeline`、`DryRunSpec`、`DryRunByID`。
     - `automation_save`：
       - 支持 `kind=pipeline`，校验 `PipelineMinIntervalMin`（15 分钟）与 Agent 创建配额。
       - `dry_run=true` 时调用 `DryRunSpec`，返回 `{ok, dry_run, errors}`，0 数据库写入。
       - `dry_run=false` 时调用 `SavePipeline`，返回 `{ok, id, next_run_at, errors}`。
       - 严格遵守规则：管线校验错误以 `ok=false` 与 `errors` 数组返回（供 Agent 读错自修），而非工具级 apperr 异常。
     - `automation_run_now`：支持 `dry_run=true`，针对 `wf_` 工作流调用 `DryRunByID`，针对 prompt 任务返回 `CodeInvalidInput`。
     - 重构守护与分派函数（`checkAgentAutomationQuota`、`checkPipelineMinInterval`、`handleDryRunSave` 等），圈复杂度与分支嵌套度满足静态分析门控。
   - `cmd/polaris/automation_bridge.go`：实现 `SavePipeline`、`DryRunSpec`、`DryRunByID` 晚绑定代理方法。
   - `internal/tool/builtin/automation_save/schema.json`：因内核 `schemavalidate` 暂不原生支持 `oneOf`，schema 采用 `kind` 枚举搭配各分支字段模式，定义 `steps`（约束 `key`、`kind` 必填）与 `trigger`、`sink` 等字段，复杂验证由内部 Validate 保证。
   - `internal/tool/builtin/automation_save/tool.yaml`：描述中补充 pipeline 最小示例与 `dry_run=true` 先行确认指导。

---

## 2. 门控验证结果

- `go build ./...`：全部构建通过。
- `go test ./internal/tool/... ./internal/automation/... ./internal/gateway/server/... ./internal/extension/mcp/... ./internal/protocol/... ./cmd/polaris/... -race -count=1`：全部 PASS，无竞态与内存异常。
- `make fmt && make lint && make docs-refs`：
  - 代码格式化无漂移。
  - 19 个质量门控（SafeDialer、Capability Token、Taint Typed Fields、FSM Effects IO、Task State、Must Check Error、SQL rows.Err()、Route Coverage、Rust FFI Coverage、TODO Inventory、Panic Ratchet、Chan Send Guard、AppErr Semantics 等）全部 PASS。
  - golangci-lint 0 issues。
  - docs-refs、comment-refs、anchor-refs 全部通过。
