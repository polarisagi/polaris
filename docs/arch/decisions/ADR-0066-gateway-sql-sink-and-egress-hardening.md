# ADR-0066: Gateway 直连 SQL 下沉 Repository（A-4）+ EgressGateway 收紧默认白名单（A-6）

## 状态
Accepted（已执行，回填）

> 本文件为 2026-07-23 复核时回填——`local_playground/upgrade/UPGRADE-PROMPT-ADVANCED.md`
> 将 A-4、A-6 明确指定为同一份 ADR-0066，且均标注"CODE 本轮"（要求本轮落码）。
> 代码改动已在本轮 35 个提交范围内完成（`internal/protocol/repo/repo_automation.go`、
> `internal/store/repo/repo_automation.go`、`cron_handlers.go` 等），但对应 ADR 文件
> 此前始终未创建。复核时发现 A-4 的六个目标站点中有三处（`cron_scheduler.go`、
> `cron_templates_handlers.go`）实际仍是裸 SQL，已在本次复核中一并补齐迁移
> （见下"复核订正"）。本文件按最终代码状态回填。

## 背景 (Context)
### A-4：Gateway 直连 SQL
Gateway 控制层多处 `ca.DB.Query*`/`h.DB.QueryRow*` 直执 SQL，绕过已存在的
`internal/protocol/repo` DAO（`AutomationRepository`/`ChannelRepository` 等），
违反 R1.1（ctrl→svc→dao 分层）/ HE-3（可组合原语，跨模块用结构化接口而非隐式耦合）。
目标站点：
- `cron_runner.go:234`（`SELECT type,config_json FROM channels WHERE id=?`）
- `cron_handlers.go:17/171/285/322`
- `cron_scheduler.go:46/92/131`（`cronTick`/`eventTick`）
- `cron_templates_handlers.go:68`（`TriggerWebhookAutomations`）
- `chat/sessions.go:19`（`SELECT id,type FROM channels`）
- `channelsadmin/webhook_receive.go:34`

范围明确限定在这六个站点；`internal/channel/manager.go:RestoreChannelsFromDB` 的
`db.QueryContext` 属 channel Manager 而非 Gateway ctrl 层，刻意不动（另案）。
Gateway 层其余 `sysadmin/{plugin,provider,workflowadmin,insightsadmin,doctor,budget,export}`
等模块的裸 SQL 使用不在 A-4 范围内，本 ADR 不覆盖、不背书、也不禁止。

### A-6：EgressGateway 默认白名单
`internal/gateway/egress/egress_gateway.go` 的 `DefaultAllowedDomains()` 此前硬编码
包含 `"localhost"`、`"127.0.0.1"`，在 M13 层显式放行环回地址，违反防御纵深与最小权限
原则（GD-9-002）。虽然 M11 SafeDialer 有兜底，但纵深防御要求出站白名单本身不应放行。

## 决策 (Decision)

### A-4
- 优先复用已存在的 Repository 方法；缺失的补最小只读方法到接口 + 实现。
- `AutomationRepository`（`internal/protocol/repo/repo_automation.go`）新增/复用：
  `ListDueAutomations`、`ListEventAutomations`、`ListWebhookAutomations`（`channelID`
  维度，`internal/store/repo/repo_automation.go` 实现，WHERE 条件与原始裸 SQL 语义
  完全对齐：`enabled=1 AND (trigger_type='webhook' OR trigger_type='both') AND
  channel_id=? AND last_run_status != 'running'`，未额外附加 `circuit_open` 过滤，
  避免语义漂移）。
- `cron_scheduler.go` 的 `cronTick`/`eventTick`、`cron_templates_handlers.go` 的
  `TriggerWebhookAutomations` 均已改为调用上述 repo 方法，移除内联 SQL/scan 代码。
- **复核订正**：Gemini 原始提交在 `cron_handlers.go`（132 行改动）等文件完成了部分
  站点迁移，但 `cron_scheduler.go`/`cron_templates_handlers.go` 三处仍是裸 SQL，
  且 `TimeoutRuns` 引用了 `automation_runs` 表中从未存在过的 `error` 列（应为
  `error_msg`，`017_automations.sql` 从未定义过 `error` 列，此前每次调用都会以
  SQL 错误静默失败，导致卡在 `running` 状态的记录永远不会被标记超时）。均已在
  2026-07-23 复核中修复并补齐迁移。

### A-6
- `DefaultAllowedDomains()` 生产默认白名单中移除 `"localhost"`、`"127.0.0.1"`。
- 本地调试场景改为运行时动态注入或测试内显式注入，不作为生产默认值。

## 结果 (Consequences)
- **正面**：Gateway 控制层与存储引擎的直接耦合在 A-4 六个目标站点上被切断，
  未来更换存储实现或加统一审计/缓存层不再需要改 Gateway 代码；修复了
  `TimeoutRuns` 的死 SQL Bug（此前从未生效）。EgressGateway 默认白名单不再放行
  环回地址，纵深防御到位。
- **负面**：`ca.DB` 字段本轮未删除（避免牵连过广），仍与新 repo 字段并存于
  `CronAdmin`/`ChannelsAdmin` 结构体，逐步替换而非一次性清空，因此
  `grep -rn "DB.Query" internal/gateway/server/` 在整个 Gateway 范围内不会归零
  ——这是刻意的范围收敛（见"背景"节），并非遗留缺陷。
- **验证**：`internal/store/repo/repo_automation_test.go`、
  `internal/gateway/server/sysadmin/cronadmin/*_test.go`、
  `internal/gateway/server/sysadmin/channelsadmin/channels_extra_test.go` 均已更新
  并通过，覆盖 cron 触发、webhook 触发、超时标记三条路径。
