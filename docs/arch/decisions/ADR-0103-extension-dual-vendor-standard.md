# ADR-0103: 扩展体系对齐 OpenAI / Anthropic 双标准（技能 / 连接器 / 插件，删除独立 App 类型）

- **状态**: Accepted | **日期**: 2026-09-26 | **模块**: M13-bis / M7 / internal/extension / internal/action/hook / internal/execute/orchestrator
- **修订**: ADR-0016 决策二（Hook 仅 5 事件 / Custom Agent 仅 YAML）、决策三（`apps` 作为 Layer 2 运行时表）

## 上下文

2026-09 业界扩展分类收敛为三层：**技能（Skill，教做事）/ 连接器（Connector，接外部系统 = MCP）/ 插件（Plugin，前两者 + hooks/agents 的分发包）**。Claude：Connectors/Skills/Plugins；ChatGPT 2026-07 以 Plugin Directory 取代 App Directory，App 降为插件子组件；Codex 插件 = skills + `.mcp.json` + `.app.json`（连接器绑定）+ hooks；agent-plugins.org 1.0（Amazon/Cursor/Microsoft/OpenAI/Vercel）定义跨厂商可移植包。

Polaris 现状（2026-09-26 审计）：`ext_type=app` + `apps`（028）表仅存 URL，全仓无 Widget 渲染，激活只注入一段描述；`AppDef{name,url,command}` 为自造格式（Codex 真实格式是 `apps.<alias>.id`）；`PluginInstaller` 不注册插件内技能、把 `.mcp.json` 当数组解析（标准 `mcpServers` map 被静默丢弃）；清单解析含自造格式（`plugin.toml`、Google `skills.yaml`、`.polaris-plugin/`）；不解析任何 `marketplace.json`；MCP 客户端停在 2025-11-25。

## 决策一：扩展分类收敛为 技能 / 连接器 / 插件（+ 市场）

- 删除 `ext_type=app`、`types.TypeApp`、`apps` 表（`028_apps.sql` 删除，编号永久留空）、`/v1/apps*` 路由、`AppRepository`、`AppInstaller`、提示词 App 摘要。
- UI「MCP」标签改名「连接器 / Connectors」；底层 `ext_type` 仍为 `mcp`（协议名不改，改的只是用户可见术语）。
- 保留的顶层 `ext_type`：`skill` / `mcp` / `plugin` / `automation` / `agent`。

## 决策二：插件清单多格式归一化（单一内部模型 `protocol.PluginManifest`）

探测顺序（同时存在时全部读取，按下表合并）：

| 优先级 | 清单 | 来源标准 | 备注 |
|---|---|---|---|
| 1 | 根 `plugin.json`（`$schema` 指向 agent-plugins.org） | Agent Plugins 1.0 | `extensions["com.openai"]` 为对象时**整体替换** `.codex-plugin/plugin.json`（OpenAI 规定，不合并） |
| 2 | `.claude-plugin/plugin.json` | Anthropic | 可缺省：无清单时按默认布局加载，`name` 取市场条目名或目录名 |
| 3 | `.codex-plugin/plugin.json` | OpenAI Codex | `interface` 为展示元数据 |

- **身份字段**（name/version/description/author/homepage/repository/license/keywords/displayName）取最高优先级清单中的非空值。
- **组件**按「默认位置 + 清单声明」逐类型解析，语义严格按来源标准：Claude `commands/agents/outputStyles` **替换**默认目录、`skills` **追加**、`hooks/mcpServers/lspServers` **合并**（后声明同名覆盖）。多清单指向同一物理路径时按解析后的绝对路径去重。
- **路径规则**（两家 + agent-plugins 一致）：组件路径必须 `./` 开头、解析后在插件根内、必须存在；含 `..` 拒绝。违规组件单独失败，不连坐其他组件（agent-plugins 一致性条款 5）。
- **未知字段**：顶层未知字段剥离 + 告警；严格对象（`userConfig` 项、`channels` 项、`lspServers` 项、`monitors` 项）含未知键 → 该组件失败。
- **路径变量**：`${PLUGIN_ROOT}` / `${CLAUDE_PLUGIN_ROOT}` → 安装根；`${PLUGIN_DATA}` / `${CLAUDE_PLUGIN_DATA}` → `<data>/extensions/plugin-data/<id>/`（升级保留、卸载删除）；`${user_config.KEY}`；stdio 子进程同时导出四个变量。展开非递归、字面替换。
- **宿主环境变量**：Claude `.mcp.json` 支持 `${VAR}` / `${VAR:-default}` 展开宿主环境变量。Polaris 只允许展开 `sanitizeParentEnv` 白名单内的变量，其余视为未定义（取默认值或空串）并告警——否则插件可用 `${OPENAI_API_KEY}` 等引用把宿主密钥注入自身进程或请求头（R1.15 / HE-7）。
- **Polaris 生成物也用标准格式**：`PluginCreator` 与市场 MCP 包装产物改为 agent-plugins 1.0 布局（根 `plugin.json` + `mcp.json` + `skills/`），删除 `.polaris-plugin/`。Polaris 私有数据只允许放在 `extensions["ai.polarisagi"]`。
- 删除自造格式解析：`plugin.toml`、`skills.yaml`/`agent-manifest.yaml`、`ai-plugin.json`（2024-04 已关闭的 ChatGPT Plugins）、`PluginBundleManifest` 的 `entrypoint`/`mcpInline`/`mcp_inline`/`hooks{install,uninstall}`/`skills[{path}]` 私有变体。

## 决策三：组件 → Polaris 运行时映射（语义可映射的全部实现）

| 组件 | 来源 | Polaris 运行时 |
|---|---|---|
| skills（`skills/<n>/SKILL.md`、清单 `skills` 路径、根 `SKILL.md` 单技能） | 两家 + agentskills.io | `skills` 表，`plugin_id` FK；决策五 |
| commands（`commands/*.md`、对象映射 `source`/`content`） | Anthropic | 转为 `user-invocable` 技能（Claude 已声明 commands 为技能旧形态），`kind=command` |
| agents（`agents/*.md` frontmatter） | Anthropic | `lifecycle.AgentDefinitionProvider` → `types.AgentProfileSpec`（来源 `plugin:<id>`），命名 `<plugin>:<agent>`；见下方「决策三补充」 |
| hooks（`hooks/hooks.json` + 清单 `hooks` + Codex `extensions.com.openai.hooks`） | 两家 | `internal/action/hook`；决策六 |
| mcpServers（`.mcp.json`、`mcp.json`、清单内联、`.json` 路径、`.mcpb`/`.dxt` 包） | 两家 + agent-plugins | `mcp_servers`，`plugin_id` FK；类型 `stdio`/`http`/`streamable-http`/`sse`/`ws` |
| userConfig | Anthropic | `plugin_user_config`；`sensitive=true` 经 `credential.Vault` 加密；提示词中敏感值替换为占位符 |
| channels | Anthropic | 绑定插件 MCP 服务器为 channel 来源（`internal/channel`） |
| dependencies / defaultEnabled | Anthropic | 同市场内解析依赖，安装按拓扑序；启用前校验依赖已启用 |
| apps（`.app.json`） | OpenAI Codex | 决策四 |
| interface（Codex）/ displayName | 两家 | `plugins` 展示元数据 |
| lspServers / outputStyles / themes / monitors / workflows / `bin/` / `settings.json` / `experimental.*` | Anthropic | **解析 + 校验，不激活**，记入 `unsupported_components` 并在 UI 标注「此宿主不适用」（agent-plugins 一致性条款 3：Ignore unsupported component types）。`bin/` 不进入任何 PATH |

### 决策三补充：子 Agent 定义与委派（2026-09-26）

- **来源与格式**：插件 `agents/`（仅 Claude `.md`）；项目 `<root>/.polaris/agents/`、用户 `<data>/agents/`（Claude `.md` + Codex `.toml`，同名 `.md` 优先）。同名优先级项目 > 用户（Claude 规则）；插件 agent 带 `<plugin>:` 命名空间。删除 Polaris 私有 `agents/*.yaml`（ADR-0016 §2.4，与决策二「删除自造格式」一致）。解析唯一实现 `pluginspec.{ParseAgentFile,ListAgentDir}`。
- **调用**：`transfer_to_agent(target_agent_role, context_summary)` 为委派入口，`list_agents` 列出本地子 Agent + `mcp:` A2A 目标 + `general-purpose`；二者注册为内置工具（此前 `transfer_to_agent` 未注册，S_VALIDATE 按未知工具拒绝，委派在生产不可达）。`DefaultTaskWorker` 把 `agent_handoff:<name>` 解析为角色规格经 `WithAgentProfile` 注入 `AcquireHeadless`；未知名称使任务失败（Claude：未知 subagent_type 报错），失败原因写入 `tasks.result` 回传委派方。
- **角色只收窄能力**，全部在内核执行入口硬拦截（`Agent.checkProfileTool`，可见性过滤仅辅助）：
  - `tools`/`disallowedTools`：Claude 工具名映射到 Polaris 内置名（`Read`→`read_file`… `Bash`→`bash`/`run_command`/`code_act:*`，`Agent`/`Task`→委派工具，`Skill`/`Skill(x)`，`mcp__server`/`mcp__server__*`）；Polaris 原生名直接有效。白名单中参数级规则（`Bash(git *)`）无法等价映射 → 整条忽略（不放宽为整个工具）；黑名单中按整个工具拒绝。`Agent(a, b)` 限定委派目标。
  - 只读：Codex `sandbox_mode="read-only"`、Claude `permissionMode: plan` → 只放行能力 ≤ `CapReadOnly` 的工具，`code_act` 与未知工具 fail-closed。其余 sandbox/permission 模式继承宿主，不得放宽。
  - `maxTurns` → 收紧 `MaxStepsLimit`。
  - 子 Agent 默认不能再委派（Claude 子 Agent 无 Agent 工具；Codex `agents.max_depth` 默认 1）；深度仍受 `SpawnDepth` 门控。
- **角色指令**写入 `ZoneMutableSkill`（`<agent_profile>`），不进 `ZoneImmutable`：用户目录 TaintLow；插件/项目目录与 AGENTS.md 同一威胁模型 TaintMedium（Spotlighting）。`skills` 字段经技能执行器渲染后预加载（插件命名空间优先），缺失即失败，不带缺口运行。
- **解析但不生效**（记入 `not_applied` + 诊断，UI 标注）：`model`/`effort`/Codex `model_reasoning_effort`（模型按阶段池路由，ADR-0101）、`background`、`isolation`、`memory`（子 Agent 共享委派方命名空间）、`initialPrompt`、agent 级 `mcpServers`/`hooks`、Codex 其余 config 覆盖键；插件 agent 的 `hooks`/`mcpServers`/`permissionMode` 按 Claude 安全规则忽略。

## 决策四：Codex「应用」= 插件内的连接器绑定，不是扩展类型

- 格式按 Codex：`.app.json` = `{"apps":{"<alias>":{"id":"<connector_id>"}}}`；由 `extensions.com.openai.apps` 或 `.codex-plugin/plugin.json` 的 `apps` 指向，默认 `./.app.json`。
- 安装时逐个 alias 在本地连接器目录（`mcp_servers` / `extension_catalog` 的 MCP 条目）按 `id` 解析：命中 → `status=bound`；未命中（例如 ChatGPT 平台专有的 `plugin_asdk_app_*` / `connector_*` id）→ `status=unbound`，UI 引导用户绑定到一个本地连接器。
- 落库 `plugin_app_bindings(plugin_id, alias, connector_ref, bound_server_id, status)`；**不**把 app 转换成 `ext_type=mcp` 条目，也不生成新的 MCP 进程。

## 决策五：技能遵循 agentskills.io 规范 + 两家扩展字段

- 校验分两级（宿主须同时运行两家技能，只有两家都拒绝的才算硬错误）：
  - **硬错误 → 技能不加载**：frontmatter 不是合法 YAML；解析后的名称为空、>64 字符、含空白 / 路径分隔符 / `..` / 控制字符。
  - **规范告警 → 加载并在 UI 展示**（agentskills.io 条款，Claude 宽容处理）：`name` 非 `[a-z0-9-]` / 首尾 `-` / 含 `--` / 与目录名不一致；`description` 缺失（按 Claude 回落为正文首个非空行）或 >1024 字符；`compatibility` >500 字符；`metadata` 非 string→string。
- `name` 缺省取目录名（Claude 规则）；插件内技能对外名为 `<plugin>:<name>`，`name` 已带本插件前缀时不重复加前缀。
- Polaris 私有技能参数（`exec_mode`、`risk_level`、`sandbox` 等）只从 `metadata` 读取（键前缀 `polaris-`），Polaris 生成的 SKILL.md 同样写入 `metadata`，保证产物可被 `skills-ref validate` 通过。
- 可选字段全量解析：`license`、`compatibility`、`metadata`、`allowed-tools`；Anthropic 扩展 `when_to_use`、`argument-hint`、`arguments`、`disable-model-invocation`、`user-invocable`、`disallowed-tools`、`model`、`effort`、`context`、`agent`、`paths`；Codex `agents/openai.yaml`（`interface`、`policy.allow_implicit_invocation`、`dependencies.tools`）。
- 调用控制：`disable-model-invocation: true` 或 `allow_implicit_invocation: false` → 不进入模型可见技能索引，只能用户显式调用；`user-invocable: false` → 仅模型可调用。
- 正文变量：`$ARGUMENTS` / `$ARGUMENTS[N]` / `$N` / `$name`、`${CLAUDE_SKILL_DIR}`、`${CLAUDE_PLUGIN_ROOT}`、`${CLAUDE_PLUGIN_DATA}`、`${CLAUDE_SESSION_ID}`。
- `` !`cmd` `` / ` ```! ` 动态注入：经统一沙箱执行（`CallerType="skill_inject"`），受 PolicyGate 约束；插件技能的注入命令与插件 hooks 同受「安装 ≠ 信任」约束（决策六）。
- `allowed-tools` 只作为该技能回合内的预授权**提示**，不绕过 Cedar/PolicyGate（HE-7）。

## 决策六：Hooks 统一为两家共同的 `hooks.json` 模型

- 格式：`{"hooks":{"<Event>":[{"matcher":"<regex>","hooks":[{"type":...}]}]}}`；处理器 `command`（含 `args` exec 形式、`async`、`timeout`、`statusMessage`）、`http`、`mcp_tool`；`prompt`/`agent` 类型走 Polaris LLM 路由（须标注用途与思考档位，ADR-0101）。
- 事件：实现 Polaris 有对应语义的全部事件——`SessionStart`、`SessionEnd`、`UserPromptSubmit`、`PreToolUse`、`PermissionRequest`、`PostToolUse`、`PostToolUseFailure`、`SubagentStart`、`SubagentStop`、`Stop`、`StopFailure`、`PreCompact`、`PostCompact`、`Notification`、`Elicitation`、`ElicitationResult`、`Interrupt`。IDE/终端专属事件（`Worktree*`、`CwdChanged`、`FileChanged`、`InstructionsLoaded`、`MessageDisplay` 等）解析但不触发，同决策三「不适用」处置。
- 输入/输出协议兼容两家：stdin JSON 带 `session_id`/`turn_id`/`hook_event_name`/`cwd`/`model`/`permission_mode` 及事件字段；退出码 0/2/其他语义；JSON 输出识别 `continue`/`stopReason`/`systemMessage`/`decision`/`reason`/`hookSpecificOutput.{permissionDecision,permissionDecisionReason,updatedInput,additionalContext}`。
- **安装 ≠ 信任**（Codex 规则，Polaris 采纳为硬约束）：插件 hooks 按定义内容 hash 记录，未经用户审阅的 hash 不执行；定义变更后重新进入待审。
- Hook 输出仍强制 `TaintLevel=High`，经 PolicyGate 决定注入，禁止进入 Immutable Zone（ADR-0016 决策二不变）。
- 实现约束（2026-09-26 落地时补充）：
  - 命令处理器经 `sandbox.RunStdio`（Rust 沙箱封装 argv，与 MCP stdio 同一能力）执行，以满足 stdin 输入与 stdout/stderr 分离；封装失败拒绝裸执行。原 `ExecEnvelope` Command 分支与 `KindHookExecute` 删除。
  - PreToolUse / PermissionRequest 为 veto-only：hook 的 `allow` 不绕过 PolicyGate 与人工审批；`updatedInput` 改写后重新执行 PolicyGate；`ask` 在无逐次交互权限通道时按拒绝处理。
  - PostToolUse 改为同步：`decision:block` 原因与 `additionalContext` 追加到工具输出并强制 TaintHigh（修订原"PostToolUse 不回写结果"）。
  - Stop `decision:block` 续跑上限 3 次（宿主硬上限，HE-5），hook 自身发起的调用不再触发 hook（防递归）。
  - 用户级 hooks.json 视为已信任；项目级与插件 hooks 按定义哈希信任。`[ShellHooks]` 私有事件与 `hooks.yaml` 删除。

## 决策七：市场格式与来源

- 读取 `.claude-plugin/marketplace.json` 与 `.agents/plugins/marketplace.json`（Codex；`.claude-plugin/marketplace.json` 亦为 Codex 旧位置）。
- 来源类型：相对路径（含 `metadata.pluginRoot` 裸名）、`local`、`github`、`url`、`git-subdir`、`npm`（禁用安装脚本）、`archive`（HTTPS + `sha256`）。`ref`/`sha` 固定版本。全部出站经 SafeDialer（XR-06）。
- **不支持 `command` 来源**：在服务端宿主上执行市场声明的任意命令越过安装安全门（HE-2/HE-7），解析后标记为不可安装并展示原因。
- Claude 条目的 `strict` 合并语义、Codex 条目的 `policy.installation`（`AVAILABLE`/`INSTALLED_BY_DEFAULT`/`NOT_AVAILABLE`）与 `policy.authentication` 按原语义实现；`INSTALLED_BY_DEFAULT` 仍须过 `Manager.InstallExtension`，不得静默安装（M13-bis §5.6）。

## 决策八：MCP 客户端对齐 2026-07-28（末阶段）

`server/discover` 无状态握手 + 对 2025-11-25 服务器回退 `initialize`；`Mcp-Method`/`Mcp-Name` 头；`resultType` 与 `InputRequiredResult` 多轮；Tasks 扩展轮询；OAuth（CIMD、`iss` 校验、按授权服务器绑定凭据）；MCP Apps（`ui://`、`text/html;profile=mcp-app`）在 Web 聊天沙箱 iframe 渲染，UI 发起的调用走同一 ExecuteTool 审计路径。

## 后果

- **正向**：两家与 agent-plugins 标准的插件可直接安装运行；删除约 1.5K 行空壳/自造格式代码；插件内技能、MCP 真正生效（修复现存静默丢失）。
- **负向**：`PluginBundleManifest` 等 `internal/protocol` 契约破坏性变更（B5.2）；hooks 引擎事件扩面需 FSM 多处埋点；分 8 个阶段落地，期间 M13-bis 描述需逐阶段订正。
- **反例守护**：
  - 拒绝恢复独立 `app` 扩展类型或 `apps` 表——App 在两家标准中都是连接器/插件子组件。
  - 拒绝把 `.app.json` 的 app 条目转成 `ext_type=mcp`——违背 Codex 绑定语义（本 ADR 决策四，2026-09-26 用户裁决）。
  - 拒绝新增 Polaris 私有清单格式或私有字段变体——私有数据只进 `extensions["ai.polarisagi"]`。
  - 拒绝「安装即信任」执行插件 hooks / 技能注入命令。
  - 拒绝以 `allowed-tools` 旁路 PolicyGate。
  - 拒绝支持市场 `command` 来源。

## 被驳回的方案

| 方案 | 驳回理由 |
|---|---|
| `.app.json` 映射为 `type=mcp` 条目 | Codex 中 app 是对平台已注册连接器的引用，不是连接定义；映射会凭空造出无端点的 MCP |
| 保留 App 为顶层类型对应 ChatGPT Apps SDK | 富 UI 已成为 MCP Apps 扩展（MCP 服务器能力），不是独立扩展类型 |
| 对 LSP/主题等实现宿主适配 | 服务端 Agent 无编辑器/终端渲染面；agent-plugins 1.0 明确允许忽略不支持的组件类型 |

## 引用代码

`internal/protocol/extensions.go`、`internal/extension/{marketplace,lifecycle,native,skill,plugin}/`、`internal/action/hook/`、`internal/extension/lifecycle/agent_definitions.go`、`internal/agent/agent_profile.go`、`internal/tool/catalog/tool_restriction.go`、`internal/tool/builtin/{list_agents,delegation_tools}*.go`、`internal/security/credential/vault.go`、`internal/protocol/schema/{008,015,018,019,020,021}_*.sql`、`docs/arch/M13-bis-Extension-Registry.md §1/§2/§5`

## 重新评估触发条件

1. agent-plugins.org 发布 1.1+ 且 Anthropic 采纳为 Claude 插件主格式 → 重议优先级表（决策二）。
2. OpenAI 发布可供自托管解析的连接器目录 API → 决策四的 `unbound` 可自动解析。
3. Polaris 新增编辑器/终端宿主形态 → 重议 LSP/outputStyles 等「不适用」组件。

## 修订记录

| 日期 | 变更 |
|---|---|
| 2026-09-26 | 初稿 |
| 2026-09-26 | 决策六：补充落地实现约束（RunStdio 执行、veto-only、PostToolUse 同步回传、Stop 续跑上限、ShellHooks 删除）。 |
| 2026-09-26 | 决策五：技能校验由「违规即不加载」改为硬错误/规范告警两级。理由：Claude 规定 `name`、`description` 均可缺省（分别回落目录名与正文首行），按 agentskills 严格拒绝会使合法 Claude 技能无法加载，违背双标准兼容目标。新增决策二补充：宿主环境变量展开限制。 |
| 2026-09-26 | 决策三补充：子 Agent 定义与委派落地（双格式解析、工具名映射、只读/步数/禁委派硬拦截、YAML 私有格式删除、`transfer_to_agent` 注册）。 |
