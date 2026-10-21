
# 模块 13-bis: Extension Registry

> 扩展系统的市场、安装、路由三层模型。覆盖 Skill / MCP（UI 称「连接器」）/ Plugin / Automation / Agent 五类扩展；分类、双厂商标准、信任模型、安装实例表见 ADR-0103。[HE-Rule-3] [HE-Rule-6]
<!-- §跳读: 0:9 职责边界 / 1:16 能力分层 / 2:30 扩展类型 / 3:67 技能执行模式 / 4:91 工具懒加载 / 5:102 安装流 / 6:192 信任门控 / 7:238 文件系统 / 8:260 调用路由 / 9:298 自动化 / 10:351 434 跨代理协作 / 11:360 460 学习技能归并 / 12:369 474 表引用 -->

---

## 0. 职责边界

- **是**: 市场同步、目录展示、安装/卸载 API、安装状态追踪；`extension_instances` 作为所有已安装扩展的单一事实来源（SSoT）；安装后运行时绑定（写 `mcp_servers` / `skills` / `plugins` / `automations`；Plugin 子 MCP / 子 Skill 带 `plugin_id` FK）；工具能力发现（ToolSearch 懒加载、Extension Card 元数据）。
- **不是**: MCP 进程生命周期管理（M7 MCPManager）/ Wasm 执行与沙箱（M7 WazeroRuntime）/ Skill 检索与 Logic Collapse（M6）/ 信任策略制定（M11 Cedar-Gate）/ 自动化任务调度（M13 Scheduler；automation 扩展的元数据在此注册）。

---

## 1. 能力分层模型

- **Layer 0 Market（目录层）**：`plugin_marketplaces`（市场来源，builtin 内置可追加）、`extension_catalog`（同步快照，只读缓存，不驱动执行）。
- **Layer 1 Instances（安装层，SSoT）**：`extension_instances` 统一记录，字段 id/ext_type/origin/catalog_id/name/publisher/trust_tier/runtime_id/install_path/config/status/error_msg。`enabled`/`parent_id` 已删除。
- **Layer 2 Runtime（运行时层）**：
  - `mcp_servers`（015）：所有 MCP 进程配置，含独立 MCP 与插件子 MCP（`plugin_id` FK）
  - `skills`（008）：script runtime 执行元数据，name 统一 `"skill:{slug}"`，含 `plugin_id` FK
  - `plugins`（021）：插件运行时状态（install_path/enabled/mcp_policy/manifest），enabled 权威源为 `mcp_servers.enabled`
  - `automations`（017）：触发器 + Agent 任务配置，M13 Scheduler 消费

**数据流**：`plugin_marketplaces → 同步 → extension_catalog → 安装 → extension_instances → 绑定 → Runtime 表`。`extension_instances` 是唯一跨层视图，前端查询、卸载全走此表。

---

## 2. 扩展类型

`app` 类型与 `apps`（028）表已删除（ADR-0103 决策四：Codex `.app.json` 按「插件内连接器绑定」处理）。

| ext_type | 核心能力 | 运行时绑定 | 典型来源 |
|----------|---------|-----------|---------|
| `mcp` | 外部工具进程（JSON-RPC 2.0 over stdio/HTTP） | `mcp_servers` → MCPManager | marketplace / user |
| `skill` | 行为指令集（SKILL.md）或 Wasm 执行单元 | `skills`（008） | marketplace / learned |
| `plugin` | Skills + MCP + Hooks 打包分发单元 | `plugins`（021）+ 子 MCP 写 `mcp_servers`、子 Skill 写 `skills`（`plugin_id`=plugins.id）；生命周期级联 | marketplace |
| `automation` | 触发器 + Agent 任务（cron/webhook/both/manual/event） | `automations`（017） | user / marketplace |
| `agent` | 外部 AI Agent 端点（A2A）暴露为工具；DDL 保留枚举，**安装器当前未实现**，见 §5.5 / §10 | — | — |

### 2.1 多厂商格式适配（ADR-0103 决策二/三/五）

插件、技能、MCP 配置的**唯一解析器**是 `internal/extension/pluginspec`（纯解析，不落库）；安装器（`lifecycle`）与 gateway 均经它读取，不得另写清单解析。

| 清单 | 标准 | 说明 |
|------|------|------|
| 根 `plugin.json`（`$schema` 指向 agent-plugins.org）+ `mcp.json` + `skills/` | Agent Plugins 1.0 | `extensions["com.openai"]` 为对象时整体替换 `.codex-plugin/plugin.json` |
| `.claude-plugin/plugin.json`（可缺省，按默认布局加载） | Anthropic | skills（追加）/ commands、agents（替换）/ hooks、mcpServers（合并）/ userConfig / channels / dependencies |
| `.codex-plugin/plugin.json` | OpenAI Codex | skills / mcpServers / apps（`.app.json`）/ hooks / interface |
| `.mcp.json` / `mcp.json` / 清单内联 / `.json` 路径 / `.mcpb`·`.dxt` 包 | 两家 + agent-plugins | 类型归一化为 stdio / http / sse / ws |
| `SKILL.md`（+ Codex `agents/openai.yaml`） | agentskills.io + 两家扩展 | 硬错误/规范告警两级 |

组件级错误逐组件隔离，记入诊断（`plugins.manifest` 快照）；LSP/主题/输出风格/monitors/workflows/`bin/`/`settings.json` 解析后标为「此宿主不适用」。`ai-plugin.json`、`plugin.toml`、Google `skills.yaml`、私有 `.polaris-plugin/` 不再作为安装格式。

### 2.2 origin 枚举

| origin | 含义 | trust_tier 默认值 |
|--------|------|-----------------|
| `builtin` | 程序内嵌生存工具（bash, search_extension, install_extension） | 4 TrustSystem |
| `marketplace` | 第三方市场（官方包为 `origin='marketplace'` + `trust_tier=3`，无 `origin='official'`） | 继承 extension_catalog |
| `user` | 用户手动创建/配置 | 1 TrustLocal |
| `learned` | M9 自演化 promote | 1 TrustLocal |

---

## 3. 技能执行模式

SKILL.md frontmatter 的 `exec_mode`（DB 同名列，DDL 仅 `'tool'|'ambient'`；"同时暴露为工具 + 注入"需注册两条记录）：

| exec_mode | 机制 | 触发时机 | 适用 |
|-----------|------|---------|------|
| `tool`（默认） | script runtime 暴露为 `skill__{slug}` LLM 工具（双下划线）；Logic Collapse 脚本经 `execute_skill`（M7 §1） | 按需，LLM 决策 | 专项任务技能 |
| `ambient` | instructions 注入每次请求的 system prompt | 会话开始自动加载 | 全局行为规范 |

**ambient 加载规则**:
- 查询 `skills WHERE exec_mode='ambient' AND deprecated=0`，按 `ambient_priority`（`always|auto|index_only`，`008_skills.sql`，默认 `auto`）与 trust_tier 排序；`always` 强制注入全文，`index_only` 仅注入索引摘要。
- 注入位置：system prompt ImmutableCore 区末尾，TaintedData 区之前。
- 总字符限制：合计 ≤ `spec/state.yaml §thresholds.m13_interface.ambient_skill_max_chars`（默认 4000，约不超 10% 上下文窗口）；超预算的降级为 index-only（仅名称+描述行并 WARN），优先保留 trust_tier 高者。链路：state.yaml → `config.M13InterfaceThresholds.AmbientSkillMaxChars` → `Server.SetAmbientSkillMaxChars` → `ChatHandler.AmbientMaxChars`，未注入回落 `defaultAmbientMaxChars`。

**代码约束**：`internal/gateway/server/chat/system_prompt_ambient.go` `buildAmbientSkillsSection()` 负责 ambient 注入；`buildToolSchemas()` 负责 tool 模式 schema（仅 `runtime='script'`）；Logic Collapse 脚本技能经 `execute_skill`，不直接进 LLM 工具列表；两条路径互不干扰。

### 3.1 标准技能的调用与渲染（ADR-0103 决策五）

- **模型侧**：`catalog.ModelToolView` 是技能以工具形态暴露的唯一视图（启动期 `skill_loader.go` 与动态 `SkillCatalog` 共用）。`model_invocable=0`（`disable-model-invocation` / Codex `allow_implicit_invocation: false`）或已废弃的技能不进模型可见列表；描述 = `description` + `when_to_use`（前缀对外名，截断 1536 字符），参数 schema 为 `{"arguments": string}`（附 `argument-hint`）。
- **用户侧**：`/插件:技能 参数`、`/技能 参数`（裸名唯一时）或 Codex `$技能 参数` 由 `SlashCommandRouter` 展开为渲染后的技能内容，作为本轮意图继续推理（`CommandResult.RewrittenInput`）；`user_invocable=0` 不可由用户触发。`GET /v1/skills/commands` 供前端补全。
- **渲染**：`pluginspec.RenderSkill` 单遍替换 `$ARGUMENTS` / `$ARGUMENTS[N]` / `$N` / 具名参数 / `${CLAUDE_SKILL_DIR}` / `${CLAUDE_PLUGIN_ROOT}` / `${CLAUDE_PLUGIN_DATA}` / `${CLAUDE_SESSION_ID}` / 非敏感 `${user_config.*}`（敏感键输出 `<sensitive:KEY>`）；未消费参数占位时追加 `ARGUMENTS:`；结果首行给出技能目录。
- **动态注入**（`` !`cmd` `` / ` ```! `）：注入点在模板上定位，命令集合哈希审阅信任后才执行（`hook_trust` 键 `skill:<技能名>`，`GET /v1/skills/injections`、`POST /v1/skills/injections/trust`）；命令参数单引号转义（防模型参数注入）；经 `sandbox.RunStdio` 在技能目录执行，失败中止整次调用（grep/diff/test 等退出码 1 除外）；注入点以哨兵占位，输出不被二次展开；未装配执行器时替换为 `[shell command execution disabled by policy]`。
- **脚本**：`script_path` 列持久化 Polaris 脚本技能入口（空=纯指令技能）；标准技能 `scripts/` 由模型经受控工具执行。

## 4. 工具发现与懒加载

已安装工具总数超过 `spec/state.yaml §thresholds.m13_interface.lazy_load_tool_threshold` 时切换懒加载，避免 context 爆炸。激活状态按 `session_id` 跟踪（`internal/tool/catalog/composite.go`）。

- **正常模式**：`buildToolSchemas()` 全量返回 builtin + mcp + skill（exec_mode=tool，runtime=script）。
- **懒加载模式**（ADR-0119，不依赖 Embedder）：仅返回静态核心集（14 个，含 `tool_search`）；其余工具在系统提示里以"名称 用途"一行索引列出，LLM 经 `tool_search(query)`（关键词/BM25，Embedder 可用时叠加语义）按需激活，命中结果（LIMIT 10）的完整 schema 在下一轮追加到 tools 数组尾部并在会话内保留。
- **ADR-0120 修订**：激活不再改动 tools 数组（`m7_tool.lazy_activation=in_history`，默认）。`tool_search(query, limit≤20)` 返回命中工具的描述与精简 schema（文本，精确名优先、未命中给最接近名称），模型随后用 `call_tool{name, arguments}` 调用；`call_tool` 在内核 S_VALIDATE 之前解包，权限/审批/污点判定均作用于内层工具。设为 `tools_array` 可恢复旧行为（激活后追加到 tools 数组，会断一次缓存）。系统提示里的工具索引为英文文本，行格式 `名称 用途` 并带 `call_tool` 用法一行。
- **5 级搜索降级**（`internal/extension/native/extension_manager.go`）：① SurrealDB FTSSearch（BM25）→ ② VecKNN（语义向量，需 embedFn）→ ③ GraphSpreadingActivation（沿 "provides" 边扩散）→ ④ SQLite `extension_catalog` LIKE → ⑤ 线上 MCP 注册表。`cognitive` 或 `db` 为 nil 时自动降级。

---

## 5. 安装流

### 5.1 安装主干（三类共用）

`Manager.InstallExtension`（授权 + 写 `extension_instances`）→ 文件就位 → `Manager.CompleteInstall` → `InstallFSM` 按 `ext_type` 分发安装器 → FSM 统一回写 `install_path` / `runtime_id` / `status`。gateway 只把市场缓存拷到 `extensions/{extID}`，不做清单解析或运行时注册；文件未就位（异步拷贝中）实例保持 `installing`，不会提前置 `installed`。

MCP 连接一律经 `MCPManager.StartFromDB(serverID)`：`mcp_servers` 行是配置权威源，`ConfigFromRow` 统一处理 `{DATA_DIR}`、插件变量（`${PLUGIN_ROOT}`/`${CLAUDE_PLUGIN_ROOT}`/`${PLUGIN_DATA}`/`${user_config.*}`/白名单宿主变量）与 `headers`。

### 5.2 MCP 连接器（独立）

目录内标准 `.mcp.json` / `mcp.json` 恰好声明一个服务器 → 写 `mcp_servers`（行 ID = 实例 ID）→ `StartFromDB`。多服务器须以插件分发。

### 5.3 Skill（独立）

`pluginspec.ParseSkillDir` → 名称 `skill:{name}`（同名已被其他实例持有则 `CodeAlreadyExists`）→ `SkillRegistry.Register`（Instructions 为正文，Polaris 私有参数只取 `metadata.polaris-*`）。标准技能无需入口脚本；存在 Polaris 脚本入口（`src/index.ts` 等）时额外静态分析 fail-closed + 风险分级。

### 5.3.1 Plugin Bundle

`pluginspec.Load` → 清理同一插件上一版本子组件（停连接、删行）→ 注册技能（`skill:{plugin}__{skill}`，命令同样转技能）→ 子 MCP 逐个 PolicyGate 授权后写 `mcp_servers`（`plugin_id` FK，保留 `${...}` 原文）→ 写 `plugins`（`manifest` 为归一化快照：诊断、不适用组件、应用绑定、hooks、agents、userConfig）→ `defaultEnabled` 时启动子 MCP。

- **Codex 应用**：安装时 `lifecycle.ResolveAppBindings` 把 `.app.json` alias 解析到本地连接器写入 `plugin_app_bindings`（bound/unbound），不生成 MCP 定义。
- **插件 `channels`**：`lifecycle.ChannelService` 经 `MCPManager.SetNotificationSink` 接收 `notifications/claude/channel`，按 `plugin_channels` 启用状态路由到专属 headless 会话；审批转发经 `hitl.GatewayImpl.SetPromptRelay`；API `GET|PUT /v1/plugins/channels`。
- **插件 `agents/`** 不单独落表：`lifecycle.AgentDefinitionProvider` 运行期按 `manifest.agents[].file` 重新解析（正文不入库，限安装目录内），与项目/用户 agents 目录合并为可委派子 Agent（`GET /v1/agents`；执行链见 M08 §12）。

**生命周期级联**：`mcp_servers.enabled` 是子 MCP 启停的唯一权威：
- **禁用**：`mcp_servers.enabled=0`、`skills.deprecated=1`，`MCPManager.Remove()` × N
- **启用**：`mcp_servers.enabled=1`、`skills.deprecated=0`，`StartFromDB()` × N
- **切换子 MCP**：`POST /v1/plugins/{id}/mcp/{serverName}/toggle`，仅操作 `mcp_servers.enabled`
- **卸载**：见 §5.7

**管理 API**：`GET /v1/plugins`（子 MCP 状态读 `mcp_servers`）；`PUT /v1/plugins/{id}`（启停，级联同步 `mcp_servers` + `skills`）；`PUT|DELETE /v1/mcp-servers/{plugin_xxx}` 返回 405——插件 MCP 须经插件管理接口操作。

### 5.3.2 插件 userConfig

选项定义来自清单（Claude `userConfig`、`channels[].userConfig`、`.mcpb` 的 `user_config`），权威源是 `plugins.manifest` 快照；取值存 `plugin_user_config`（021，`scope` 区分插件级与 channel 服务器级）。`sensitive` 值经 `credential.Vault` 加密落库，API（`GET/PUT /v1/plugins/{id}/config`）只回报 `is_set`；Vault 不可用拒绝写入敏感值。`MCPManager.ConfigFromRow` 经 `PluginVarsResolver` 展开 `${user_config.*}`（channel 级覆盖插件级，默认值兜底），必填缺失则拒绝启动该服务器并返回缺失键名。保存后自动重启该插件已启用的子 MCP。

### 5.3.3 插件设置向导与快捷提示（ADR-0103 沿革 2026-10-17）

插件需要用户在界面里完成一次性设置（如安装浏览器扩展）时，在清单私有命名空间声明两个**插件内裸工具名**（解析在 `pluginspec.loadSetupTools`，进入 `Plugin.SetupTool/StatusTool`，随 `plugins.manifest` 快照落库）：

```json
"extensions": { "ai.polarisagi": { "setupTool": "bridge_setup", "statusTool": "bridge_status" } }
```

- `setupTool`：返回带 MCP Apps UI（工具定义 `_meta.ui.resourceUri`，或结果 `_meta.ui.resourceUri`）的设置视图；`statusTool` 必须配合 `setupTool`，返回 `structuredContent.ready`(bool)，可选 `message`(string)。单独出现的 `statusTool` 忽略并出诊断 `polaris.setup.statusWithoutSetup`。
- 工具可见性：`_meta.ui.visibility` 含 `app` 或 `model` 即可；仅 `app` 的工具不进模型目录，只能经本入口调用。
- 裸名 → 注册名：`lifecycle.ResolvePluginTool` 按安装期固定的服务器 ID 前缀 `plugin_<pluginID>_` 在该插件的运行时服务器里按 MCP 原始工具名查找，注册名为 `mcp__<scopedServerName>__<sanitize(tool)>`；一个插件可带多个子 MCP，不能靠拼接插件名推断。
- `Interface.DefaultPrompt`（Codex `interface.defaultPrompt`，字符串或数组）归一为 `Plugin.DefaultPrompts`：去空白/空串、≤3 条（超出截断并出诊断 `codex.interface.defaultPrompt`）。

**接口**（`internal/gateway/server/plugin/setup_handlers.go`）：

| 接口 | 说明 |
|------|------|
| `GET /v1/plugins` | 每项新增 `defaultPrompts: []string` 与 `setup: {tool, statusTool, available, reason}`（未声明 setupTool 则无 `setup`）；`reason` ∈ `not_enabled`/`dependency_unmet`/`tool_not_found`/`tool_not_visible`。清单快照来自安装时；升级前已安装的插件需重装/升级才带新字段 |
| `POST /v1/plugins/{id}/setup` | body `{"session_id"?}`。校验已安装、已启用、依赖满足（`EnableBlocker`）、工具已连接且可见 → `AppToolExecutor.ExecuteAppTool(注册名, "{}", TaintMedium, viewID, sessionID)`（与聊天回合同一执行体，principal=MCPApp，不经 LLM、不计 token）→ `types.BuildToolUIRef` → `session.SaveToolUIView` + `SaveMessage(assistant)` + `session.LinkToolUIViews` → `{session_id, message_id, view_id}`。错误体 `{"error","reason"}`：404 `not_installed`/`tool_not_found`/`session_not_found`，409 `not_enabled`/`dependency_unmet`，403 `tool_not_visible`，502 `no_ui_resource`/`tool_failed`，500 `persist_failed`（落库失败）/`load_failed`（读插件行失败或清单快照损坏），503 `not_configured`。仅 `sql.ErrNoRows` 视为 `not_installed`，其它 DB 读错误不得伪装成未安装。无 `session_id` 时失败不留空会话：视图的 `session_id` 外键要求会话先存在，故先 `EnsureSession` 再落视图/消息/关联，其后任一步失败由 `SetupStore.DeleteSession`（`chat_app_views`/`chat_messages` 均 `ON DELETE CASCADE`）回滚该会话；回滚用脱离取消的 5s 上下文，删除失败仅记日志、不覆盖原始错误；调用方传入的已有 `session_id` 失败时不删除 |
| `GET /v1/plugins/{id}/setup-status` | 执行 `statusTool`（5s 超时，viewID/sessionID 置空）→ `{ready: bool, message}`；未声明、插件未就绪、工具失败、超时、缺 `ready` 一律 `{ready: null, reason?}`（200，前端轮询不应见 5xx）；DB 读错误与清单快照损坏同属此列（`reason=load_failed`）；插件确实不存在 404 |

前端（`web/src/pages/plugins.html` + `store/plugins.js`）：已安装插件卡片展示 `defaultPrompts`（点击新建会话并 `chat.submit`）、「设置」按钮（`setup.available=false` 时禁用并显示 `plugins_setup_reason_*`）；对声明了 `setupTool` 的依赖插件，有 `statusTool` 时仅 `ready===false` 报"依赖插件 {name} 需要完成设置"，无 `statusTool` 只给设置入口。声明了 `statusTool` 的插件自身卡片在「设置」按钮旁显示就绪标记（`ready=true` 已就绪 / `false` 需要设置 / `null` 不显示），`message` 作悬停提示，未就绪时同时以小字展示；依赖提示行在 `ready===false` 时同样展示依赖返回的 `message`。自身与依赖共用 `loadSetupHints` 的同一份 readyCache（同一插件一轮只查一次），随 `load()` 刷新，不另设轮询。

### 5.4 Automation

① Cedar Gate 验证 → ② 写 `extension_instances`（`ext_type=automation`）→ ③ INSERT `automations`（DDL 见 `017_automations.sql`）→ ④ Scheduler.Register 按 `trigger_type`：cron 写调度表达式、webhook 生成触发端点、event 订阅 `events`（`eventTick`）、manual 仅响应 POST。安装统一经 `Manager.InstallExtension()`。专属 `github` trigger_type 枚举未做，GitHub 事件经通用 webhook（`internal/gateway/server/sysadmin/channelsadmin/webhook_receive.go` 的 `HandleWebhookReceive`，`verifyWebhookSource` 支持 `X-Hub-Signature-256` HMAC-SHA256）。

### 5.5 Agent（外部 AI Agent）

**设计，当前未实现**（无 `ext_type=agent` 安装器，无 `transport='a2a'` 的 MCP 行）。设计流程：Cedar Gate（TrustTier 严格校验）→ 写 `extension_instances` → INSERT `mcp_servers(transport='a2a')` → A2A Discover 取 Agent Card 转 MCP tool schema → 以 `"agent:{id}"` 注入 InProcessSandbox。现行跨代理出站走 MCP 工具 `a2a_delegate`（ADR-0111 决策一）。

### 5.6 市场同步（只同步不安装）

启动时 `bootMarketplaceInit` 后台拉取 `is_builtin=1` 市场源至 `extension_catalog`，仅作前端展示缓存，**不静默安装任何外部扩展**。同步只经标准目录（ADR-0103 决策七，私有「边界探测」爬虫与 `catalog.json` 私有格式已删除）：

- `plugin` 市场：`pluginspec.GetMarketplace` 读 `.claude-plugin/marketplace.json` / `.agents/plugins/marketplace.json`，每条目一行（`RegistryEntry.Entry` 为条目原文，`MarketplaceName`/`AllowCrossDeps` 供依赖解析）；Codex `NOT_AVAILABLE` 不列出；保留名市场拒绝；直链 JSON 市场跳过相对路径条目。
- `skill` 市场：agentskills 仓库每个 `SKILL.md` 目录一行（`pluginspec.ParseSkillDir`）。
- `mcp` 市场：MCP Registry（`server.json`）分页全量，远程端点优先，其次 npm→npx / pypi→uvx / oci→docker。
- 目录安装 `marketplace.CatalogInstaller`：`SourceFetcher` 取回来源（相对路径拷贝跳过符号链接；github/url/git-subdir 只接受 https、禁 file 协议；npm 校验 sha512 integrity、不跑脚本；archive 校验 sha256）→ 依赖先装（同市场按名；跨市场须根市场白名单；版本范围按 `<name>--v<ver>` 标签；循环依赖失败）→ `CompleteInstall`（条目经 `LoadOptions.Entry` 按 strict 与插件清单组合）。升级先取回到暂存目录再替换。

### 5.7 彻底卸载

1. `Manager.UninstallExtension`：实例置 `uninstalling` 并投递 outbox `extension_uninstall`。
2. `sandbox.ExtensionUninstallHandler` → `InstallFSM.UninstallRuntime`：插件停全部子 MCP、删 `mcp_servers`/`skills`/`plugins` 行、删 `${PLUGIN_DATA}`；独立连接器停连接删行；独立技能删行。
3. 运行时清理成功后 `os.RemoveAll(install_path)` + 删 `extension_instances`；失败则保留现场并置 `error`。

插件私有 `hooks.install` / `hooks.uninstall` 已删除；标准生命周期 hooks 由 hook 引擎按事件执行（ADR-0103 决策六）。

### 5.8 Plugin 自动生成（PluginCreator）

`internal/extension/plugin/plugin_creator.go` 的 `PluginCreator.GeneratePlugin()` 按意图生成 TypeScript MCP 服务器，以 agent-plugins 1.0 布局落盘 `~/.polaris/extensions/local/{name}/`（`plugin.json` + `mcp.json` + `src/index.ts` + `deno.json`），再以 `ext_type=plugin`、`LocalPath` 走 §5.1 主干——与市场插件同一解析、授权与启动路径。运行时优先 Deno（`deno run --no-prompt` + `denoPermFlags`），不可用回退 `npx tsx`。

---

## 6. 信任门控

> 策略制定见 M11 Cedar-Gate；本节只描述触发点。TrustTier 五级定义 → ADR-0103 决策九。

**核心约束**：所有安装路径（手动、Agent 自治、AI 生成）必须通过 `Manager.InstallExtension` 中央网关，不可绕过。

| trust_tier | 安装时 | 运行时 |
|------------|-------|-------|
| 4 System   | 不走此流（程序内嵌） | 直接执行 |
| 3 Official | 自动确认 | Sbx-L2，TaintMedium |
| 2 Community | 自动确认 | Sbx-L1，TaintHigh |
| 1 Local    | 用户确认 | Sbx-L1，TaintHigh |
| 0 Untrusted | 拒绝 | — |

安装时 trust_tier 强制从 `extension_catalog` 继承，禁止客户端覆盖。Plugin hooks 存在时 trust_tier < 3 触发 HITL 审批。

### 6.1 所有安装入口必须过门——禁止并行旁路

每条写入 `mcp_servers` / `extension_instances` 的 HTTP 端点都必须独立调用 `Manager.InstallExtension`，不得以"父路径已审查"为由跳过：

| 端点 | 必须过门 | 备注 |
|------|---------|------|
| `POST /v1/plugins/install` | ✅ | — |
| `POST /v1/mcp/create` | ✅ | — |
| `POST /v1/mcp-servers`（运维管理） | ✅ **不可例外** | 调 `installMgr.Authorize` + `InstallExtension` |
| `PUT /v1/plugins/{id}` | ✅ | 修改 `mcp_servers.enabled` 后实时 Remove/start |
| `POST /v1/plugins/{id}/mcp/{serverName}/toggle` | — | 仅 enabled 切换，不重走 InstallExtension（§6.3） |
| `PUT /v1/mcp-servers/{id}`（更新） | ✅ | — |

### 6.2 安全门 nil 不等于可选

`Manager` 经依赖注入传入。**`if installMgr != nil { gate }` 之后继续执行是 R1.14 反模式**——nil 时必须返回 503，不得静默绕过。

### 6.3 Plugin Bundle 子组件门控

Bundle 子组件写入全局表但**只过一次门**：
- **安全边界**：① 父插件安装时的 Cedar Gate（含 hooks 风险评估）；② `trust_tier` 继承自 `plugins` 表，写入 `mcp_servers.trust_tier` 与 `skills.trust_tier`。
- 子 MCP 的 PUT/DELETE 受限（405），防止绕过插件管理接口。
- toggle 切换启停属修改已授权安装范围，不重走 InstallExtension。

### 6.4 HasHooks 判断规则

市场安装路径在下载前无法读取 plugin.json，hooks 存在性不可知。**保守策略**：`plugin` 类型且 `trust_tier < 3` 时 `HasHooks=true`，强制触发 HITL；trust_tier ≥ 3（Official）方可豁免。

---

## 7. 文件系统布局

```
~/.polaris/
├── extensions/
│   ├── {ext_id}/               # 市场安装的技能 / 插件（gateway 从市场缓存原样拷贝）
│   │   ├── SKILL.md            # 独立技能（Polaris 参数在 metadata.polaris-*）
│   │   ├── .claude-plugin/ | .codex-plugin/ | plugin.json   # 插件：三种清单任一或并存
│   │   ├── skills/ commands/ agents/ hooks/hooks.json .mcp.json .app.json
│   │   └── .mcpb-cache/        # .mcpb / .dxt 包解压位置
│   ├── local/{name}/           # PluginCreator 自动生成（agent-plugins 1.0 布局）
│   ├── plugin-data/{plugin_id}/ # ${PLUGIN_DATA}：跨升级保留，卸载删除
│   └── agent/{ext_id}/         # Agent Card 缓存（设计）
├── hooks/                      # 全局钩子（Plugin Bundle 安装 + 用户配置）
├── cache/{marketplace_id}/     # 市场下载临时区（安装后清理）
└── data/polaris.db
```

`extension_instances.install_path`：skill/plugin 与市场安装的连接器为绝对路径；手工配置的 mcp、automation、agent 为空串。

---

## 8. 调用路由

### 8.1 工具列表构建（每次推理请求）

`totalTools() ≤ lazy_load_tool_threshold`（`spec/state.yaml §thresholds.m13_interface`）时全量返回 builtin + mcp + script runtime skill（exec_mode=tool）；超限仅返回核心集（含 `tool_search`），其余见系统提示索引（ADR-0119）。`skillToolSchemas()` 仅暴露 `runtime='script' AND exec_mode='tool'` 的技能，工具名 `skill__{slug}`（DB 键 `skill:{slug}` 去前缀加双下划线）；Logic Collapse 脚本技能经 `execute_skill`，不进此列表。

### 8.2 工具执行路由（toolExec closure）

- `skill__{slug}` → DB 读 `skills.instructions`（script runtime，LLM 执行 instructions）
- `execute_skill` → MCPManager → ContainerSandbox.Execute（Rust 沙箱）
- `agent:{id}` → 设计路径（Cedar Gate → A2A Client → 外部 Agent，结果赋 TaintHigh）；当前跨代理走 `mcp__<server>__a2a_delegate`（§10）
- `tool_search` → 查询未加载的工具，命中后激活到当前会话（下一轮可调用）
- 其他 → `sandboxRouter.Execute` → InProcessSandbox（builtin 直接执行，mcp 走 `CallToolTainted()`）

### 8.3 Ambient Skill 注入（每次推理请求）

`injectSystemPrompt` 查 `exec_mode='ambient' AND deprecated=0`。用 **Tier 2 语义向量**（Rust `vec_cosine_f32` purego FFI，`internal/ffi/vec_ops.go`）计算 User Prompt 与各 Ambient Skill 的余弦相似度；向量引擎不可用则平滑降级为 **Tier 1 关键词匹配**。命中阈值（默认 `0.60`）的 Skill 激活并按 trust_tier 排序，总量受 §3 的 `ambient_skill_max_chars` 约束，注入 ImmutableCore 区末尾。

### 8.4 MCP Async Tasks（MCP spec 2025-11-25）

耗时 MCP 工具（预估 >5s）走异步：`MCPManager.CallToolAsync()` 立即返回 `{task_id, status=pending}`，内部 goroutine 监控完成后写 `tasks_cache`（内存 map，TTL=300s，后台每 30s 淘汰过期条目，`internal/extension/mcp/async_tasks.go`）。LLM 侧调用面：`registerTools`（`internal/extension/mcp/mcp_manager_tools.go`）为每个 MCP 工具额外注册 `<原工具名>_async` 变体（fire-and-forget，返回 `{task_id, status}`）；`get_task_result` 是独立 builtin（`internal/tool/builtin/get_task_result/`），按 task_id 轮询任意来源异步任务。架构边界：`tool/builtin` 属 L1、`extension/mcp` 属 L2，`get_task_result` 依赖同包 consumer-side 接口 `AsyncTaskProvider`（HE-3），由 `cmd/polaris/adapters_mcp_async.go` 在 main 包适配桥接 `*mcp.MCPManager`。

### 8.5 MCP 客户端协议（2026-07-28 双纪元，ADR-0103 决策八）

| 能力 | 实现（`internal/extension/mcp/`） | 要点 |
|---|---|---|
| 纪元探测 | `mcp_client_era.go` | 先 `server/discover`，失败回退 2025-11-25 `initialize`；新纪元每请求 `_meta`（protocolVersion / clientInfo / clientCapabilities，含 tasks 与 ui 扩展声明） |
| 请求头 | `mcp_headers.go` | `MCP-Protocol-Version`；新纪元 `Mcp-Method` / `Mcp-Name` / `Mcp-Param-*`（`x-mcp-header` 标注，非 ASCII 走 base64 哨兵）；旧纪元回传 `Mcp-Session-Id` |
| 多轮输入 | `mcp_client_mrtr.go`、`elicitation.go` | `input_required` 统一输入处理器（sampling / elicitation / roots 空列表），最多 8 轮；HeaderMismatch(-32020) 刷新工具头后重试一次；elicitation 经网关 broker 与 `Elicitation` / `ElicitationResult` hooks |
| Tasks 扩展 | `mcp_client_tasks.go` | 工具调用 `resultType:"task"` 转轮询（200ms~30s，TTL 兜底），`input_required` 同 key 只答一次，ctx 取消尽力发送取消；taskId 不持久化 |
| 工具变更 | `mcp_manager_refresh.go`、`mcp_client_subscribe.go` | 旧纪元自发通知、新纪元 `subscriptions/listen`（HTTP 长连接，异常断开 1s~60s 退避，优雅关闭不重连）统一进 `refreshTools`；同服务器合并为至多补一轮 |
| OAuth | `oauth_*.go` | 401/403 挑战 → PRM(RFC 9728) → AS 元数据(RFC 8414/OIDC)；注册：预注册 > CIMD(https 非回环) > DCR；PKCE S256 + `resource` + `iss` 校验；令牌 Vault 加密（`040_mcp_oauth.sql`），刷新同服务器串行化并携带客户端凭据 |
| MCP Apps | `mcp_apps_*.go` | 工具 `_meta.ui`（兼容 `openai/outputTemplate`、`openai/widgetAccessible`）；app-only 工具只进注册表隐藏集合，View 调用经 `ExecuteAppTool` 与模型调用同一执行体；视图持久化 `chat_app_views`；沙箱代理独立端口异源 |

网关侧：`/v1/elicitations`、`/v1/mcp-servers/{id}/oauth*`、`/oauth/{mcp/callback,client-metadata.json}`、`/v1/mcp-apps/*`；路由清单见 M13 §1.2.9 生成块。

---

## 9. 自动化（Automation Extension）

自动化是**有触发器的 Agent 任务**，第一类扩展类型（`ext_type='automation'`）。**prompt 是自包含的任务规约**（声明目标与成功标准），Agent 在独立上下文执行，结果推送至指定目标；每次执行产生独立 session，与主聊天隔离。用户侧页面、触发模型与 Webhook 验签见 M13 §8.7。

### 9.1 数据模型

DDL `internal/protocol/schema/017_automations.sql`。核心字段：`prompt`、`trigger_type`（cron/webhook/both/manual/event）、`cron_schedule`、`event_filter`、`env_type`、`working_dir`、`reasoning_effort`、`result_action`（session/channel:{id}/silent）、`sandbox_level`、`cedar_rules_json`、`next_run_at`（cronTick 预计算索引）、`last_run_status`（ok/error/running 防重入）、`created_at`/`updated_at`。
执行历史 `automation_runs`（同文件）：`trigger`、`status`（running/ok/error/timeout）、`session_id`（关联 chat_sessions）、`prompt_snapshot`（执行时快照，防修改后无法追溯）。

### 9.2 执行环境（env_type）

| env_type | 说明 | Git 隔离 |
|----------|------|---------|
| `inline`（默认） | 读写 `working_dir`（空则纯对话无文件访问） | 无 |
| `worktree` | Git worktree 隔离，执行后可生成 PR | ✓ `auto/{date}/{task_id}`（`internal/automation/worktree.go`） |

**禁止**：`model_id` 不对 automation 暴露——系统按 `reasoning_effort` 映射 model_roles。

### 9.3 触发路径

| trigger_type | 机制 |
|---|---|
| `cron` | cronTick 60s 轮询，`last_run_status != 'running'` 防重入，`next_run_at <= NOW()` 触发 |
| `webhook` | `POST /v1/webhooks/{channelType}/{channelID}`，HMAC-SHA256 验签（密钥存 CredentialVault） |
| `both` | cron + webhook 两路独立，互不阻塞 |
| `manual` | `POST /v1/automations/{id}/trigger`，202 Accepted + `{run_id}`，异步执行 |
| `event` | `eventTick` 轮询 `events` 表按 `event_filter` 匹配 |

`calcNextRun` 支持 5 字段 cron（含 `*/n`）+ 别名（@hourly/@daily/@weekly/@monthly）+ 完整 day/weekday 匹配。

### 9.4 执行流（executeAutomation）

1. 写 `automation_runs`（`status='running'`，`prompt_snapshot`）
2. 后台 goroutine：timeout 按 `reasoning_effort`（low=5m / medium=15m / high=30m / ultra=60m），创建独立 `chat_sessions`
3. 注入 ImmutableCore（含 `env_type / working_dir / cedar_rules_json`）
4. StreamInfer：独立推理上下文，禁污染主会话
5. 按 `result_action` 处理结果
6. 更新 `automation_runs` 与 `automations` 状态

**不变量**：automation 必须使用独立 sub-inference 上下文（`inv_M13_03` cron pool 隔离），禁止注入主聊天上下文。

### 9.5 工作流（Workflow）

`workflows` / `workflow_steps` 主从表（`029_workflows.sql`）已实现**链式（chain）与 DAG（dag）**两种编排：`workflow_steps.depends_on` 构成真实依赖图（AND-Join 并行边），`max_retries` 失败重试，`executeWorkflow` 经 `StateGraphExecutor` 执行（`internal/gateway/server/sysadmin/workflowadmin/workflow_graph.go`）。语义、数据交接与续跑见 M13 §8.7；执行器设计 → ADR-0046 决策三。

### 9.6 防重入与 HITL 审批

- **防重入**：cronTick 查询加 `AND last_run_status != 'running'`。
- **HITL**：触发危险操作（WriteNetwork / Privileged / 超预算）→ M11 Cedar-Gate 拦截 → `automation_runs.status='suspended'` → SSE 推 `event:approval_pending` → 用户在 `/automation` 页"待办审批"处理 → `POST /v1/approvals/{id}/resolve` → 恢复或取消。
- **禁止**：automation 不得自动降级绕过 Cedar-Gate（`inv_M11_02`）。

---

## 10. 跨代理协作（Agent Extension + A2A）

`agent` 扩展类型（外部 AI Agent 以工具形式暴露，Agent Card 经 `/.well-known/agent-card.json` 发现）的安装器当前未实现（§5.5）。现行跨 Agent / 跨节点协作 → ADR-0111：
- **出站**：对端 Agent 建模为 MCP 工具 `a2a_delegate`（寻址 `mcp:<server>/<agent>`，复用 `transfer_to_agent` 挂起通道，`MCPA2AWorker` 认领），强制 TaintHigh、PII 出境脱敏、`spawn_depth`+`chain` 防环。
- **入站**：A2A 1.0 门面 `/a2a/v1` 与 MCP 门面 `/mcp`；`GET /.well-known/agent-card.json` 由 M13 Gateway 生成（基于已安装 skills + mcp_servers 能力描述）。
- **多节点管理**：`/v1/mesh/*`，设置页「多节点」Tab（M13 §8.2）。

---

## 11. 学习技能归并（M9 → Extension Registry）

M9 promote 候选技能时：
1. 写 `extension_instances`（`ext_type=skill, origin=learned, trust_tier=1`）
2. INSERT `skills`（`runtime='script'`，`instructions`=生成的 SKILL.md，`exec_mode='tool'`）
3. `install_path` 指向 `extensions/skill/learned/{ext_id}/`

**禁止**：M9 不得绕过 `extension_instances` 直写 `skills`（inv_M6_02）。技能经足够次数成功调用后，Logic Collapse 将其蒸馏为 Python 脚本（M6 §2.2，ADR-0008）：`UPDATE skills SET runtime='script'`，`src/skill.py` 写入安装目录，走 `SkillExecutor.ExecuteSkill()`（ContainerSandbox L3）。

## 12. 表引用速查

| 表 | 迁移 | 消费方 |
|----|------|-------|
| `plugin_marketplaces` | 018 | M13 API（市场注册） |
| `extension_catalog` | 019 | M13 API（目录缓存） |
| `extension_instances` | 020 | M13 API（安装 SSoT；已删 `enabled`、`parent_id`） |
| `mcp_servers` | 015 | M7 `MCPManager.LoadFromDB()`；含 `plugin_id`、`work_dir` |
| `skills` | 008 | M6 SkillRegistry + `buildToolSchemas()` + `buildAmbientSkillsSection()`；含 `plugin_id` |
| `plugins` / `plugin_user_config` / `plugin_channels` / `plugin_app_bindings` | 021 | bundle 元数据 / userConfig / channels / 应用绑定；`mcp_policy` 仅存附加策略 |
| `automations` / `automation_runs` | 017 | M13 Scheduler |
| `workflows` / `workflow_steps` / `workflow_runs` | 029 | M13 工作流 |
| （014 永久留空） | 014 | 原定时任务表已彻底废弃删除（ADR-0114） |

`skill_sources` 已删除，职责归入 `extension_instances`。

**关键关系**：`mcp_servers.plugin_id → plugins.id`、`skills.plugin_id → plugins.id`（卸载插件级联删除）；`extension_instances.runtime_id` 指向 runtime 表 PK：`mcp_servers.id` | `skills.name` | `plugins.id`。
