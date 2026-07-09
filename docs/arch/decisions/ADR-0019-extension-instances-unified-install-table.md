# ADR-0019: extension_instances 统一安装实例表

**状态**: Accepted
**日期**: 2026-05-22
**取代**: skill_sources（历史草案编号023）、plugins（历史027）、apps（历史028）三表散乱安装记录。以上编号为 ADR 起草时的历史临时编号，最终已随 ADR-0019 执行重整为标准编号体系（001–021）。当前 DDL 权威目录参见 `internal/protocol/schema/`。
**相关**: ADR-0016（TrustTier）、M13-bis-Extension-Registry.md

---

## 背景

现有安装记录散落在四个表中：`skill_sources`、`plugins`、`apps`、`mcp_servers`（通过 `catalog_id` 标记已安装）。导致三个结构性问题：

1. **安装状态查询需 UNION 四表**：`getInstalledCatalogIDs` 发四条 SQL 才能得到完整视图。
2. **安装断层**：`installSkillSource` 只写 `skill_sources`，未下载文件、未写 `skills`（008）运行时表，SkillExecutor 永远找不到该 skill。
3. **026_skills.sql 是历史草案死代码**：008 已建 `skills` 表，026 的 `CREATE TABLE IF NOT EXISTS skills` 永远不执行，但其意图（目录级 skill 记录）无人承接。上述所有历史编号均已随 ADR-0019 执行重整消除。

---

## 决策

新增 `extension_instances` 表（迁移文件 `020`），作为所有已安装扩展的单一事实来源。

**关键字段**：
- `ext_type`：`mcp` | `skill` | `plugin` | `app`
- `origin`：`builtin` | `marketplace` | `user` | `learned`
- `catalog_id`：关联 `extension_catalog.id`；user/learned 时为空
- `runtime_id`：安装完成后写入，指向 `mcp_servers.id` 或 `skills.name`
- `install_path`：文件系统绝对路径；MCP/App 为空字符串
- `status`：`downloading` | `installed` | `error` | `disabled`

删除 `skill_sources` 表的 DDL。曾经废除过 `apps` 表，但在后续（2026-06-01 对齐 Codex App 概念）决议中**撤销了废除 apps 表的决定**。`apps` 表（028）被恢复，专门作为富交互前端应用（Web UI/Widget）的独立运行时表。`plugins` 表（021）作为 Plugin Bundle 的专用运行时记录被保留，负责管理插件的包清单（manifest）和子 MCP 启停策略（mcp_policy）。至此，所有扩展的安装状态统归 `extension_instances`（Layer 1），而运行参数（Layer 2）被严格拆分到 `mcp_servers`（仅限独立 MCP）、`skills`（仅限独立技能）、`plugins`（聚合插件包）和 `apps`（独立交互应用）四表中，插件内的子组件不再跨越边界污染基础表。Schema 整体重整为 001-028 标准编号。

---

## 被拒绝的方案

| 方案 | 拒绝原因 |
|------|---------|
| 按类型分四个安装表（mcp_installs / skill_installs ...） | 前端需 UNION 查询，安装状态无法单表追踪 |
| 直接用 `mcp_servers` / `skills` 表的 `catalog_id` 标记安装 | 运行时配置表与安装元数据职责混淆，skill 无法记录 `install_path` 和 `status` |
| 保留 `skill_sources`，只修复安装流 | 不消除表语义重叠，`getInstalledCatalogIDs` 仍需多表 UNION |

---

## 影响

- `internal/gateway/server/plugin/catalog.go`：`installSkillSource` / `handleUninstallPlugin` / `appendCustomCatalogs` 重写
- `internal/protocol/schema/`：`020_extension_instances.sql`（主表）；相关运行时表均在原始 DDL 中直接包含所需字段（原计划的 034/035/036 迁移补丁文件未创建。`extension_instances` 表及相关字段变更均已内嵌于 020/021/028 等原始 DDL 文件，直接删库重建生效）。
- `M13-bis-Extension-Registry.md`：安装流完整描述
- M9 Self-Improvement Engine：promote 路径必须经 `extension_instances` → `SkillRegistry`（inv_M6_02）

---

## 架构演进 (2026-06-02)

本节内容已被整合。关于插件子组件统一写入全局 Runtime 表的三层架构统一（State-in-DB）及生命周期级联同步机制，当前详见 [M13-bis-Extension-Registry](../M13-bis-Extension-Registry.md) 中的说明。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-05-22 | 初稿，Accepted |
| 2026-06-13 | DDL 迁移文件状态更新：所有变更已内嵌于原始 DDL 文件 |
