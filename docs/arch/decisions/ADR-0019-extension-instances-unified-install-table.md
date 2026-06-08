# ADR-0019: extension_instances 统一安装实例表

**状态**: Accepted
**日期**: 2026-05-22
**取代**: skill_sources（历史草案编号023）、plugins（历史027）、apps（历史028）三表散乱安装记录
> 注：以上编号为 ADR 起草时的历史临时编号，最终已随 ADR-0019 执行重整为标准编号体系（001–021）。当前 DDL 权威目录参见 `internal/protocol/schema/`。
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

- `pkg/gateway/server/plugin_catalog.go`：`installSkillSource` / `handleUninstallPlugin` / `appendCustomCatalogs` 重写
- `internal/protocol/schema/`：新增 034、035、036 迁移文件
- `M13-bis-Extension-Registry.md`：安装流完整描述
- M9 Self-Improvement Engine：promote 路径必须经 `extension_instances` → `SkillRegistry`（inv_M6_02）

---

## 架构演进 (2026-06-02)：插件子组件统一写入全局 Runtime 表

**背景**：原始设计（2026-06-01）要求”插件内嵌组件不跨边界注入全局表”，依靠 `LoadFromPlugins` / `LoadOnePlugin` 在运行时从文件系统动态加载，用 `appendPluginMCPServers` 动态拼接 MCP 列表。经过 agentskills.io 开放标准调研（对齐 OpenAI Codex 和 Anthropic Claude Code），该设计被推翻。

**新架构（三层统一，State-in-DB）**：

1. **插件安装时同步写入全局 Runtime 表**（agentskills.io 标准做法）：
   - 子 MCP → `mcp_servers`（新增 `plugin_id` + `work_dir` 列）
   - 子 Skill → `skills`（新增 `plugin_id` 列）
   - 两表均通过 `plugin_id = plugins.id` 关联，支持级联同步。

2. **`extension_instances` 精简为纯安装元数据表**：
   - 删除 `enabled` 列（废字段，从未被 runtime 消费；启用/禁用状态由各 runtime 表自管理）
   - 删除 `parent_id` 列（原设计的 bundle 子记录机制，未实现；现由 runtime 表 FK 替代）
   - `status` 枚举缩减为 `downloading` | `installed` | `error`（`disabled` 废弃）

3. **MCPManager 简化**：删除 `LoadFromPlugins` / `LoadOnePlugin`，统一由 `LoadFromDB` 加载（查 `mcp_servers WHERE enabled=1`，含插件子 MCP）。

4. **插件生命周期级联同步**：
   - 禁用插件 → `UPDATE mcp_servers SET enabled=0 WHERE plugin_id=?` + `UPDATE skills SET deprecated=1 WHERE plugin_id=?` + MCPManager.Remove
   - 启用插件 → 反向同步 + 重新启动各子 MCP
   - 卸载插件 → `DELETE FROM mcp_servers WHERE plugin_id=?` + `DELETE FROM skills WHERE plugin_id=?`（硬删除，不留脏数据）

5. **监控展示层契约**（不变）：配置（DB）＋ 运行时状态（MCPManager 内存快照）叠加渲染。`mcp_servers` 表现为唯一查询来源，无需 `appendPluginMCPServers` 动态拼接。

**被替换的机制**：
- `MCPManager.LoadFromPlugins` / `LoadOnePlugin` → 删除
- `appendPluginMCPServers` → 删除（统一 LEFT JOIN `mcp_servers` 查询）
- `plugins.mcp_policy` 中的 `enabled` 键 → 废弃（`mcp_servers.enabled` 是权威来源）
- 独立 skill name 格式 `sk_{hex}` → 统一为 `skill:{hex}`，经 `SkillRegistry.Register` 写入

**参考**：agentskills.io 开放标准（Anthropic 发布，Claude Code / OpenAI Codex / GitHub Copilot 等共同采纳）
