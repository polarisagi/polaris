# 模块 13-bis: Extension Registry

> 扩展系统的市场、安装、路由三层模型。覆盖 MCP / Skill / Plugin / App / Automation / Agent 六类扩展。[HE-Rule-3] [HE-Rule-6]
> **§跳读**: 0:8 职责边界 / 1:22 能力分层 / 2:41 扩展类型 / 3:79 技能执行模式 / 4:105 工具懒加载 / 5:130 安装流 / 6:202 信任门控 / 7:248 文件系统 / 8:278 调用路由 / 9:311 自动化 / 10:383 跨代理协作 / 11:409 学习技能归并 / 12:436 表引用

---

## 0. 职责边界

- **是**: 市场同步、目录展示、安装/卸载 API、安装状态追踪
- **是**: `extension_instances` 作为所有已安装扩展的单一事实来源（SSoT）
- **是**: 安装后运行时绑定（写 `mcp_servers` / `skills` / `plugins` / `apps` / `automations`；Plugin 子 MCP 写 `mcp_servers`，Plugin 子 Skill 写 `skills`，均带 `plugin_id` FK）
- **是**: 工具能力发现（ToolSearch 懒加载、Extension Card 元数据）
- **不是**: MCP 进程生命周期管理（M7 MCPManager）
- **不是**: Wasm 执行与沙箱（M7 WazeroRuntime）
- **不是**: Skill 检索与 Logic Collapse（M6）
- **不是**: 信任策略制定（M11 Cedar-Gate）
- **不是**: 自动化任务调度（M13 Scheduler，但 automation 扩展类型的元数据在此注册）

---

## 1. 能力分层模型

**Layer 0 Market（目录层）**：`plugin_marketplaces`（市场来源注册，builtin 内置可追加）、`extension_catalog`（市场同步快照，只读缓存，不驱动执行）。

**Layer 1 Instances（安装层，SSoT）**：`extension_instances` 是所有已安装扩展的统一记录，字段含 id/ext_type/origin/catalog_id/name/publisher/trust_tier/runtime_id/install_path/config/status/error_msg。注：`enabled`/`parent_id` 已废弃删除。

**Layer 2 Runtime（运行时层）**：
- `mcp_servers`（015）：所有 MCP 进程配置，含独立 MCP 和插件子 MCP（plugin_id FK）
- `skills`（008）：script runtime 执行元数据，name 格式统一为 `"skill:{slug}"`，含 plugin_id FK
- `plugins`（021）：插件运行时状态（install_path/enabled/mcp_policy/manifest），enabled 权威源为 mcp_servers.enabled
- `apps`（028）：富交互应用（Codex App），runtime_id 指向此表
- `automations`（017）：触发器 + Agent 任务配置，M13 Scheduler 消费方

**数据流**：`plugin_marketplaces → 同步 → extension_catalog → 安装 → extension_instances → 绑定 → Runtime 表`

`extension_instances` 是唯一跨层视图。前端查询、卸载全走此表。

---

## 2. 扩展类型

| ext_type | 核心能力 | 运行时绑定 | 典型来源 |
|----------|---------|-----------|---------|
| `mcp` | 外部工具进程（JSON-RPC 2.0 over stdio/HTTP） | `mcp_servers` → MCPManager | marketplace / user |
| `skill` | 行为指令集（SKILL.md）或 Wasm 执行单元 | `skills`（008） | marketplace / learned |
| `plugin` | Skills + MCP + Hooks 的打包分发单元 | `plugins`（021）+ 子 MCP 写 `mcp_servers`（plugin_id=plugins.id）+ 子 Skill 写 `skills`（plugin_id=plugins.id）；生命周期级联 | marketplace |
| `app` | 独立的图形交互界面（Web UI/Widget），参考 Codex App 概念 | `apps`（028）；拥有独立的 URL 端点和权限状态 | marketplace / user |
| `automation` | 触发器 + Agent 任务（cron/webhook/both/manual；规划：event/github） | `automations`（017） | user / marketplace |
| `agent` | 外部 AI Agent 端点（A2A 协议）暴露为工具 | `mcp_servers`（transport=a2a） | marketplace / user |

### 2.1 多厂商格式适配

市场插件包（`.tar.gz`）内的清单文件通过 `pkg/extensions/marketplace/adapter.go` 统一解析为 `RegistryEntry`：

| 清单文件 | 厂商 | 安装结果 |
|---------|------|---------|
| `ai-plugin.json`（api.type=mcp） | OpenAI | mcp_servers，启动 MCP 进程 |
| `ai-plugin.json`（api.type=openapi） | OpenAI | app 类型，URL + OpenAPI schema 存储 |
| `.claude-plugin/plugin.toml` / `plugin.toml`（含 command） | Anthropic | mcp_servers |
| `.claude-plugin/plugin.json` | Anthropic | plugin 类型 |
| `skills.yaml` / `agent-manifest.yaml`（含 command） | Google | mcp_servers |
| `skills.yaml`（无 command，含 name） | Google | skills（script runtime） |

Polaris 原生格式（`SKILL.md` / `plugin.json`）由 `pkg/extensions/marketplace/loader.go` 处理。

### 2.2 origin 枚举

| origin | 含义 | trust_tier 默认值 |
|--------|------|-----------------|
| `builtin` | 程序内嵌生存工具（bash, search_extension, install_extension） | 4 TrustSystem |
| `official` | 官方市场推荐包 | 3 TrustOfficial |
| `marketplace` | 第三方社区市场 | 继承 extension_catalog |
| `user` | 用户手动创建/配置 | 1 TrustLocal |
| `learned` | M9 自演化 promote | 1 TrustLocal |

---

## 3. 技能执行模式

Skill 有两种执行模式，在 SKILL.md frontmatter 的 `exec_mode` 字段声明（DB 列名相同）：

| exec_mode | 机制 | 触发时机 | 适用场景 |
|-----------|------|---------|---------|
| `tool`（默认） | script runtime：暴露为 `skill__{slug}` LLM 工具（双下划线前缀）；Logic Collapse 脚本：经 `execute_skill` 工具调用（M7 §1）| 按需，LLM 决策 | 专项任务技能（代码审查、PR 规范） |
| `ambient` | 将 instructions 注入每次请求的 system prompt | 会话开始时自动加载 | 全局行为规范（输出格式、安全检查） |

> **注**：DDL `exec_mode` 仅支持 `'tool'|'ambient'` 两值。"同时暴露为工具 + 注入"的场景通过分别注册两条记录实现，无独立 `both` 值。

**ambient 加载规则**：
- 查询 `skills WHERE exec_mode='ambient' AND deprecated=0`，按 trust_tier 排序
- 注入位置：system prompt ImmutableCore 区末尾，TaintedData 区之前
- 总字符限制：ambient skills 合计 ≤ `m13_ext.ambient_skill_max_chars`（默认 4000 字符，不得占用超过 ~10% 上下文窗口）
- 超限时优先保留 trust_tier 高的，其余截断并 WARN

**代码约束**：
- `server.go injectSystemPrompt()` 负责 ambient 注入
- `buildToolSchemas()` 负责 tool 模式的 schema 构建（仅 `runtime='script'`）
- ⚠️ **当前缺口**：`pkg/gateway/server/sse.go:buildAmbientSkillsSection()` 缺少 `ORDER BY trust_tier DESC` 和 4000 字符截断，修复见 GEMINI_PATCH_ROUND14 问题 B
- Logic Collapse 脚本技能经 `execute_skill` 工具调用，不直接注入 LLM 工具列表
- 两条路径互不干扰

---

## 4. 工具发现与懒加载 **[计划中]**

当已安装工具总数超过 `spec/state.yaml §thresholds.m13_ext.lazy_load_tool_threshold`（默认 40），切换到懒加载模式，避免 context 爆炸（懒加载逻辑当前尚未实现，`buildToolSchemas()` 始终全量返回）：

**正常模式**（tools ≤ LazyLoadThreshold）：`buildToolSchemas()` 全量返回所有 builtin + mcp + skill（exec_mode=tool，runtime=script）的 schema。

**懒加载模式**（tools > LazyLoadThreshold）：`buildToolSchemas()` 仅返回核心 builtin 工具（trust_tier=4）和 `search_tools` 元工具；LLM 通过 `search_tools(query)` 按需发现并激活具体工具。

**search_tools 元工具**（builtin, trust_tier=4）：

```json
{
  "name": "search_tools",
  "description": "搜索并激活可用工具/技能。返回匹配的工具 schema，激活后本次对话可调用。",
  "parameters": {
    "query": "string",
    "type": "string? // mcp|skill|builtin|any"
  }
}
```

执行：`SELECT name,description FROM (mcp_schemas UNION skills UNION builtins) WHERE ... LIKE '%query%' LIMIT 10`，将命中结果的完整 schema 注入后续 tool_use 可用列表。

---

## 5. 安装流

### 5.1 MCP

Cedar Gate 验证 → 写 extension_instances（status=installing）→ INSERT mcp_servers（继承 trust_tier）→ MCPManager.startMCPServer()（goroutine 连接 + 工具注册 InProcessSandbox）→ UPDATE extension_instances（status=installed，runtime_id=mcp_servers.id）。

✅ **已实现**。

### 5.2 Skill

Cedar Gate → 写 extension_instances（status=downloading）→ 复制文件到 install_path → 解析 SKILL.md frontmatter（name/description/exec_mode/risk_level/sandbox/capability）→ SkillRegistry.Register（name="skill:{hex}"，强制 `skill:` 前缀校验）→ UPDATE extension_instances（status=installed）。exec_mode='tool' 以 "skill__{slug}" 工具名注入 LLM；exec_mode='ambient' 注入 system prompt。

✅ **已实现**。

**名称规范**：独立安装的 skill 使用 `"skill:{ext_id后缀}"` 格式（全局唯一，不依赖 SKILL.md 的 name 字段）。插件 skill 使用 `"skill:{plugin-name}/{skill-slug}"` 格式（人类可读，在命名空间内唯一）。两者均通过 `SkillRegistry.Register`，强制 `skill:` 前缀校验。

### 5.3 Plugin Bundle

**设计原则（agentskills.io 开放标准，对齐 OpenAI Codex / Claude Code）：Plugin 是容器，子组件安装时同步写入全局 Runtime 表，通过 `plugin_id` FK 关联，生命周期级联管理。**

Cedar Gate（含 hooks 安全检查）→ 写 extension_instances（status=downloading）→ 复制文件 → 解析 plugin.json（收集子 MCP 定义和子 Skill）→ INSERT plugins(021) → 子 MCP 写 mcp_servers（id="plugin_{pl_id}_{name}"，plugin_id FK，work_dir=install_path）+ MCPManager.Add() 异步连接 → 子 Skill 写 SkillRegistry（name="skill:{plugin-name}/{slug}"，plugin_id FK）→ UPDATE extension_instances（status=installed）。

启动时 MCPManager.LoadFromDB 统一加载含 plugin_id 的子 MCP；Skill 注入走 `buildToolSchemas()`（tool 模式）和 `buildAmbientSkillsSection()`（ambient 模式）。

> **✅ 已修复（extension_instances 双写脑裂）**：`plugin_catalog.go` 中 `internalInstallMCP` / `internalInstallGeneric` 的裸 `INSERT INTO extension_instances` 已删除，所有写路径统一路由至 `marketplace.Manager.InstallExtension`，实现 ADR-0019 单写原则；`Manager` 新增 `UpdateStatus` 方法供状态变更使用。

**插件生命周期级联**：

插件生命周期级联操作均通过标准 API 执行，`mcp_servers.enabled` 是子 MCP 启停的唯一权威：
- **禁用**：`UPDATE mcp_servers SET enabled=0`，`UPDATE skills SET deprecated=1`，MCPManager.Remove() × N
- **启用**：`UPDATE mcp_servers SET enabled=1`，`UPDATE skills SET deprecated=0`，startMCPServer() × N
- **切换子 MCP**：`PATCH /v1/plugins/{id}/mcp/{name}` 操作 `mcp_servers.enabled`（不操作 mcp_policy）
- **卸载**：MCPManager.Remove() + 硬删除 mcp_servers/skills/plugins + os.RemoveAll(install_path) + 删 extension_instances

**管理 API**：
```
GET    /v1/plugins                      已安装插件列表（子 MCP 状态从 mcp_servers 读取，不再解析 mcp_policy）
PUT    /v1/plugins/{id}                 启用/停用插件（级联同步 mcp_servers + skills）
PATCH  /v1/plugins/{id}/mcp/{name}     切换子 MCP（操作 mcp_servers.enabled，不允许独立 DELETE/PUT）
DELETE /v1/mcp-servers/{plugin_xxx}    返回 405——插件 MCP 须通过插件管理接口操作
```

### 5.4 Automation

Cedar Gate → 写 extension_instances（ext_type=automation）→ INSERT automations（trigger_type/trigger_config/action_type/action_ref）→ Scheduler.Register（按 trigger_type 注册：cron 写调度表达式，webhook 生成 trigger 端点，event 订阅 outbox，manual 仅响应 POST）。

> ⚠️ **代码偏差**：Automation 安装流经由 `Manager.InstallExtension()` 的路径同 §5.3 描述的空壳问题。cron/webhook/manual 触发已实现；**event 触发 ✅ 已实现**（`eventTick` 轮询 `events` 表，按 `event_filter` JSON 匹配 topic/type，复用 `cronTick` 周期执行，`017_automations.sql` 新增 `event_filter TEXT` 列）；github 触发仍为计划中。

### 5.5 Agent（外部 AI Agent）

Cedar Gate（TrustTier 严格校验）→ 写 extension_instances（ext_type=agent）→ INSERT mcp_servers（transport='a2a'）→ MCPManager 通过 A2A Client Discover 获取 Agent Card 并转换为 MCP tool schema → 以 "agent:{id}" 注入 InProcessSandbox。

### 5.6 市场同步（只同步不安装）

启动时 `bootMarketplaceInit` 后台拉取 `is_builtin=1` 市场源至 `extension_catalog`，仅作前端展示缓存。**不静默安装任何外部扩展**。

**边界探测 (Bundle Root Detection)**：同步爬虫（`discoverMarketplaceEntries`）在扫描市场仓库时，一旦探测到合法的插件清单文件（如 `plugin.json`、`plugin.toml`、`mcp.json`、`skills.yaml` 等），即判定该目录为一个**原子级插件包（Plugin Bundle）**，将其整体作为单个条目录入，并强制停止向下钻取其子目录。这避免了内部依附的零碎动作（如 `SKILL.md`）被摊平暴露到全局市场，彻底杜绝列表污染与大模型工具的全局同名冲突。

### 5.7 彻底卸载

按 ext_type 清理运行时（mcp/skill/plugin/app/automation/agent 各有对应删除路径）→ os.RemoveAll(install_path)（内部经 safeJoin 路径校验）→ DELETE extension_instances → 非 builtin 来源级联 cleanCatalog()。plugin 类型额外级联删除 mcp_servers + skills（plugin_id FK）。

### 5.8 Plugin 自动生成（PluginCreator）

用户以自然语言描述意图，`PluginCreator` 调用 LLM 生成 TypeScript MCP 插件并写入本地文件系统。

`PluginCreator.GeneratePlugin()` 接收自然语言意图，调用 LLM 生成 TypeScript MCP 插件（`src/index.ts` + `package.json` + `.polaris-plugin/plugin.json` + `.mcp.json`），写入 `~/.polarisagi/polaris/extensions/local/{name}/`，返回 pluginDir（调用方负责后续注册到 extension_instances）。

**语言约定**：官方插件仓库（polaris-plugins-official）及 PluginCreator 生成的插件均采用 TypeScript + `@modelcontextprotocol/sdk`，通过 `npx tsx` 直接运行，无需预编译。社区插件可使用任意语言，加载层（loader/marketplace）格式无关，仅读 `.mcp.json` 的 `command/args`。

---

## 6. 信任门控

> 策略制定见 M11 Cedar-Gate。本节仅描述触发点。

**核心约束**：所有安装路径（手动、Agent 自治、AI 生成）必须通过 `Manager.InstallExtension` 中央网关，不可绕过。

| trust_tier | 安装时 | 运行时 |
|------------|-------|-------|
| 4 System   | 不走此流（程序内嵌） | 直接执行 |
| 3 Official | 自动确认 | Sbx-L2，TaintMedium |
| 2 Community | 自动确认 | Sbx-L1，TaintHigh |
| 1 Local    | 用户确认 | Sbx-L1，TaintHigh |
| 0 Untrusted | 拒绝 | — |

安装时 trust_tier 强制从 extension_catalog 继承，禁止客户端覆盖。Plugin hooks 存在时 trust_tier < 3 触发 HITL 审批。

### 6.1 所有安装入口必须过门——禁止并行旁路

系统存在多条写入 `mcp_servers` / `extension_instances` 的 HTTP 端点，**每一条**都必须独立调用 `Manager.InstallExtension`，不得以"父路径已审查"为由跳过：

| 端点 | 必须过门 | 常见违规写法 |
|------|---------|------------|
| `POST /v1/plugins/install` | ✅ | — |
| `POST /v1/mcp/create` | ✅ | — |
| `POST /v1/mcp-servers`（运维管理接口） | ✅ **不可例外** | 直接写库，无 PolicyGate |
| `PUT /v1/plugins/{id}` / `PATCH /v1/plugins/{id}/mcp/{name}` | ✅ | 修改 mcp_servers.enabled 后实时 Remove/startMCPServer |
| `PUT /v1/mcp-servers/{id}`（更新） | ✅ | — |

### 6.2 安全门 nil 不等于可选

`Manager` 通过依赖注入传入。**`if installMgr != nil { gate }` 之后继续执行的写法是 R1.14 反模式**——nil 时必须返回 503，不得静默绕过。安全门是强制路径，不是可选优化。

### 6.3 Plugin Bundle 子组件门控

Plugin Bundle（`§5.3`）安装时子组件写入全局表，但**只过一次门**：

- **安全边界**：① 父插件安装时的 Cedar Gate（包含 hooks 风险评估）；② `trust_tier` 继承自 `plugins` 表，写入 `mcp_servers.trust_tier` 和 `skills.trust_tier`。
- **子 MCP 的 DELETE/PUT 受限**：`DELETE /v1/mcp-servers/{plugin_xxx}` 返回 405，防止绕过插件管理接口直接删除。
- **`PATCH /v1/plugins/{id}/mcp/{name}` 切换启停**：操作 `mcp_servers.enabled`，属于修改已授权安装范围，不重走 InstallExtension。

### 6.4 HasHooks 判断规则

市场安装路径在下载前无法读取 plugin.json，因此 hooks 存在性无法确认。**保守策略**：`plugin` 类型且 `trust_tier < 3` 时，`HasHooks` 置 `true`，强制触发 HITL 审批。trust_tier ≥ 3（Official）的插件方可豁免。

---

## 7. 文件系统布局

```
~/.polarisagi/polaris/
├── extensions/
│   ├── skill/{ext_id}/         # script runtime 技能安装目录
│   │   ├── SKILL.md            # frontmatter: name, description, mode
│   │   └── src/index.ts        # Logic Collapse 蒸馏脚本（存在时为 script runtime）
│   ├── plugin/{ext_id}/        # Plugin Bundle 解压（市场安装）
│   │   ├── plugin.json         # PluginBundleManifest
│   │   ├── skills/             # Bundle 内技能
│   │   └── hooks/              # Bundle 内钩子脚本
│   ├── local/{name}/           # PluginCreator 自动生成（TypeScript）
│   │   ├── src/index.ts        # MCP 服务器实现
│   │   ├── package.json        # npm 清单（tsx + @modelcontextprotocol/sdk）
│   │   ├── .polaris-plugin/
│   │   │   └── plugin.json     # Polaris 原生清单
│   │   └── .mcp.json           # { command:"npx", args:["tsx","src/index.ts"] }
│   └── agent/{ext_id}/         # Agent Card 缓存
│       └── agent-card.json
├── hooks/                      # 全局钩子（来自 Plugin Bundle 安装 + 用户配置）
├── cache/{marketplace_id}/     # 市场下载临时区（安装后清理）
└── data/
    ├── polaris.db
```

`extension_instances.install_path`：skill/plugin 为绝对路径，mcp/automation/agent 为空字符串。

---

## 8. 调用路由

### 8.1 工具列表构建（每次推理请求）

懒加载阈值见 `spec/state.yaml §thresholds.m13_ext.lazy_load_tool_threshold`（默认 40）。

`totalTools() ≤ LazyLoadThreshold` 时全量返回 builtin + mcp + script runtime skill（exec_mode=tool）；超限时仅返回核心 builtin + `search_tools` 元工具。`skillToolSchemas()` 仅暴露 runtime='script' AND exec_mode='tool' 的技能，工具名格式为 `skill__{slug}`。Logic Collapse 脚本技能经 `execute_skill` 工具调用，不进入此列表。

> `skillToolSchemas()` 仅暴露 `runtime='script' AND exec_mode='tool'` 的技能，工具名格式为 `skill__{slug}`（DB 存储键 `skill:{slug}` 去掉前缀后加双下划线）。Logic Collapse 脚本技能经 `execute_skill` 工具调用，不进入此列表。

### 8.2 工具执行路由（toolExec closure）

`toolExec` 按工具名路由：
- `skill__{slug}` → DB 读 skills.instructions（script runtime，LLM 执行 instructions）
- `execute_skill` → MCPManager → ContainerSandbox.Execute（script runtime，Rust 沙箱）
- `agent:{id}` → Cedar Gate → A2A Client → 外部 Agent 端点（结果赋 TaintHigh）
- `search_tools` → 查询工具库，返回命中 schema 激活到当前会话
- 其他 → sandboxRouter.Execute → InProcessSandbox（builtin 直接执行，mcp 走 CallToolTainted()）

### 8.3 Ambient Skill 注入（每次推理请求）

`injectSystemPrompt` 从 skills 表查询 exec_mode='ambient' AND deprecated=0（按 trust_tier DESC 排序），拼接后截断至 4000 字符（超限按 trust_tier 优先级保留），注入 system prompt ImmutableCore 区末尾。

### 8.4 MCP Async Tasks（MCP spec 2025-11-25）

对耗时 MCP 工具（预估 > 5s），MCPManager 支持异步任务模式：

`MCPManager.CallToolAsync()` 立即返回 `{task_id, status=pending}`，LLM 通过 `get_task_result(task_id)` 轮询；内部 goroutine 监控完成后写入 `tasks_cache`（内存 map，TTL=300s）。

`tasks_cache` 为内存 map（task_id → result），超时 TTL = 300s。

---

## 9. 自动化（Automation Extension）

自动化是**有触发器的 Agent 任务**，是第一类扩展类型（ext_type='automation'）。设计参考 Codex Automations + Claude Code Routines 理念：**automation prompt 是自包含的任务规约**（须声明目标与成功标准），Agent 在独立上下文中执行，结果推送至指定目标。这与"对话延续"根本不同——每次执行产生独立 session，与主聊天互相隔离。

### 9.1 数据模型

DDL 见 `internal/protocol/schema/017_automations.sql`。核心字段：`prompt`（自包含任务规约）、`trigger_type`（cron/webhook/both/manual）、`cron_schedule`、`working_dir`、`reasoning_effort`、`result_action`（session/channel:{id}/silent）、`sandbox_level`、`cedar_rules_json`、`next_run_at`（cronTick 预计算索引）、`last_run_status`（ok/error/running 防重入）、`created_at`/`updated_at`（审计时间戳，自动生成）。

**执行历史表** `automation_runs`（同 017 文件）：每次触发产生一条 run 记录，包含 `trigger`（触发类型）、`status`（running/ok/error/timeout）、`session_id`（关联 chat_sessions，可跳入查看执行过程）、`prompt_snapshot`（执行时 prompt 快照，防 prompt 修改导致追溯困难）。

### 9.2 执行环境（env_type）

参考 Codex Automations 的三种执行模式（`worktree / local / direct`）与 Claude Code Routines 的 `repositories` 概念：

| env_type | 说明 | 工作目录 | Git 隔离 | 对应 Sandbox |
|----------|------|---------|---------|------------|
| `chat` | 纯 Agent 对话，无文件访问 | 无 | 无 | L1 InProcess |
| `local` | 读写 working_dir（项目文件） | `working_dir` | 无（直写主分支） | L2 Wasm |
| `worktree` | Git worktree 隔离，执行后可生成 PR | 自动创建临时 worktree | ✓ `auto/{date}/{task_id}` | L2 Wasm + Git |

> `env_type` 当前通过 `working_dir` 隐式表达（空=chat，非空=local）。`worktree` 模式为目标设计，需在 DDL 增加 `env_type TEXT NOT NULL DEFAULT 'chat'`，代码实现时同步创建 worktree 并在完成后生成 PR。

**禁止**：`model_id` 不对 automation 暴露——系统根据 `reasoning_effort` 自动映射 model_roles（用户不感知模型名）。

### 9.3 触发路径

| trigger_type | 实现状态 | 机制 |
|---|---|---|
| `cron` | ✅ 已实现 | cronTick 60s 轮询，`last_run_status != 'running'` 防重入，`next_run_at <= NOW()` 触发 |
| `webhook` | ✅ 已实现 | POST /v1/webhooks/{channelType}/{channelID}，HMAC-SHA256 验签（密钥存 CredentialVault） |
| `both` | ✅ 已实现 | cron + webhook 两路独立触发，互不阻塞 |
| `manual` | ✅ 已实现 | POST /v1/automations/{id}/trigger，响应 202 Accepted + {run_id}，异步执行 |
| `event` | ⚠️ 计划中 | Outbox Worker 订阅 events.type |
| `github` | ⚠️ 计划中 | Webhook + GitHub event 过滤（PR/Release + author/label/branch/regex） |

calcNextRun 支持：5 字段 cron 表达式（含 `*/n` 步长）+ 别名（@hourly/@daily/@weekly/@monthly）+ 完整 day/weekday 匹配。

### 9.4 执行流（executeAutomation）

`executeAutomation` 执行流：写 automation_runs（status='running'，prompt_snapshot）→ 后台 goroutine（timeout 按 reasoning_effort：low=5m/medium=15m/high=30m/ultra=60m）创建独立 chat_sessions → 注入 ImmutableCore（含 env_type/working_dir/cedar_rules_json）→ StreamInfer（独立推理上下文，禁污染主会话）→ 按 result_action 处理结果（session/channel:{id}/silent）→ 更新 automation_runs 和 automations 状态。

**不变量**：automation 必须使用独立 sub-inference 上下文（`inv_M13_03` cron pool 隔离），禁止注入主聊天上下文。

### 9.5 工作流（Workflow）

当前实现通过单一 prompt 指令 Agent 内部完成多步任务（Agent 自主调用工具→技能→MCP 形成流程）。这是"隐式工作流"——Agent 是流程编排器。

结构化工作流（显式 DAG）为目标设计，将多个 Action 按依赖图顺序编排，每步输出作为下一步输入：

```json
{
  "steps": [
    { "id": "s1", "type": "mcp_tool", "ref": "github:list_prs", "input": {} },
    { "id": "s2", "type": "skill",    "ref": "code_review",     "input": { "prs": "{{s1.output}}" }, "depends_on": ["s1"] },
    { "id": "s3", "type": "channel",  "ref": "slack:notify",    "input": { "summary": "{{s2.output}}" }, "depends_on": ["s2"] }
  ],
  "on_error": "stop"
}
```

DAG 执行器复用 M4 `dag_executor.go`（`pkg/cognition/kernel/dag_executor.go`）。实现时 automations 表新增 `workflow_json TEXT DEFAULT ''`，非空时走 DAG 路径替代 StreamInfer。

### 9.6 防重入与 HITL 审批

**防重入**：cronTick 查询加条件 `AND last_run_status != 'running'`，避免同一 automation 并发执行。

**HITL 审批**：automation 执行触发危险操作（WriteNetwork / Privileged / 超预算）→ M11 Cedar-Gate 拦截 → automation_runs.status = 'suspended' → SSE push `event:approval_pending` → 用户在 `/automation` 页"待办审批"Tab 处理 → POST /v1/approvals/{id}/resolve → 恢复或取消执行。

**禁止**：automation 不得自动降级绕过 Cedar-Gate（`inv_M11_02`）。

---

## 10. 跨代理协作（Agent Extension + A2A）

`agent` 扩展类型将外部 AI Agent 以工具形式暴露给本地 LLM：

安装时获取远端 Agent Card（`/.well-known/agent-card.json`），解析 capabilities/skills/authentication，INSERT mcp_servers（transport='a2a'），MCPManager.Add() 注册 A2AClientConfig，以 `"agent:{serverID}"` 暴露到 InProcessSandbox。

LLM 调用 `"agent:{serverID}"` 时走 toolExec → A2A Client → POST `{AgentCard.url}/tasks/send`，支持 streaming/async，结果返回为 ToolResult。

**Agent Card 标准字段**（遵循 A2A Protocol）：

```json
{
  "name": "string",
  "description": "string",
  "url": "https://...",
  "version": "1.0.0",
  "capabilities": { "streaming": true, "pushNotifications": false },
  "skills": [{ "id": "skill_id", "name": "...", "description": "..." }],
  "authentication": { "schemes": ["Bearer"] }
}
```

本地 Agent 对外暴露 Agent Card：`GET /.well-known/agent-card.json` → 由 M13 Gateway 自动生成（基于已安装 skills + mcp_servers 的能力描述）。

---

## 11. 学习技能归并（M9 → Extension Registry）

M9 Self-Improvement Engine promote 候选技能时：

1. 写 `extension_instances`（ext_type=skill, origin=learned, trust_tier=1）
2. 直接 INSERT `skills` 表（runtime='script'，instructions=生成的 SKILL.md，mode='tool'）
3. install_path 指向 `extensions/skill/learned/{ext_id}/`

**禁止**：M9 不得绕过 extension_instances 直写 skills 表（inv_M6_02）。

技能经过足够次数成功调用后，Logic Collapse 将其蒸馏为 TypeScript 脚本（M6 §2.2）：
- 脚本生成完成 → UPDATE skills SET runtime='script'，src/index.ts 写入安装目录
- 脚本技能走 SkillExecutor.ExecuteSkill()（Rust 沙箱执行）

---

## 11.2 已知实现缺口

| 缺口 | 严重度 | 说明 |
|------|--------|------|
| `Manager.InstallExtension()` | ✅ 已实装 | 已实现下载文件、写 extension_instances、调用 SkillRegistry |
| Automation/Agent 安装流 | ✅ 已修复 | 统一经 Manager 安装路径，cron/webhook/manual/event 触发已实现 |
| `Registry.ListEnabled` 同步 | ✅ 已修复 | 改为基于 SQLite extension_instances 表注册表管理，无需 ScanDir |
| ScanDir 路径约定 | ✅ 已废弃 | 完全转为数据库注册表，ScanDir 逻辑已移除 |

---

## 12. 表引用速查

| 表 | 迁移文件 | 消费方 | 新增字段（2026-06） |
|----|---------|-------|-------------------|
| `plugin_marketplaces` | 018 | M13 API（市场注册） | — |
| `extension_catalog` | 019 | M13 API（目录缓存） | — |
| `extension_instances` | 020 | M13 API（安装 SSoT）；**已删 `enabled`、`parent_id`** | — |
| `mcp_servers` | 015 | M7 MCPManager.LoadFromDB() | `plugin_id`、`work_dir` |
| `skills` | 008 | M6 SkillRegistry + buildToolSchemas() + buildAmbientSkillsSection() | `plugin_id` |
| `plugins` | 021 | plugin_catalog.go（bundle 元数据）；mcp_policy 仅存附加策略 | — |
| `apps` | 028 | M13 API；extension_instances.runtime_id 指向此表 | — |
| `automations` | 017 | M13 Scheduler（`pkg/gateway/server/cron.go`） | — |
| `automation_runs` | 017 | M13 Scheduler — 执行历史 | — |
| `cron_jobs` | 014 | 旧版定时任务表，由 017_automations 接管，逐步废弃 | — |

**已删除**（不再存在）：`skill_sources`——职责归入 `extension_instances`（020）。

**关键关系（2026-06 新增）**：
- `mcp_servers.plugin_id → plugins.id`：插件子 MCP，卸载插件时级联删除
- `skills.plugin_id → plugins.id`：插件子 Skill，卸载插件时级联删除
- `extension_instances.runtime_id` 指向对应 runtime 表 PK：`mcp_servers.id` | `skills.name` | `plugins.id` | `apps.id`
