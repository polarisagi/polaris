# ADR-0103: 扩展体系：OpenAI / Anthropic 双标准（技能 / 连接器 / 插件）、统一信任模型、MCP 传输

> 状态：Accepted（决策一~十三均已落地） ｜ 首次：2026-09-26（原 ADR-0016/0017/0002 为 2026-05） ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0016、ADR-0017 决策一/二、ADR-0002
> 模块：M13-bis / M7 / `internal/extension` / `internal/action/hook`。目标：Claude、Codex、agent-plugins.org 1.0 的包可直接安装运行；私有数据只进 `extensions["ai.polarisagi"]`。

## 旧引用映射（代码注释里的「ADR-0016/0017/0002/0084」按此跳转；各节标题亦含原编号）

| 旧引用 | 新节 | 旧引用 | 新节 |
|---|---|---|---|
| 0016 决策一（§2.1，TrustTier） | 决策九 | 0017 决策一/二 | 决策十一 |
| 0016 决策二（§2.x Hook / agentskills / Custom Agent） | 决策六 / 五 / 三补充 | 0017 决策三 | ADR-0111（A2A；不在本文） |
| 0016 决策二 §2.5（CSV fan-out）、P3 不做项 | 决策十三 | 0002 | 决策十二 |
| 0016 决策三/四（实例表 / 升级） | 决策十 | 0084 | ADR-0111 |

## 决策一：分类收敛为 技能 / 连接器 / 插件（+ 市场）

- 删除 `ext_type=app`、`types.TypeApp`、`apps` 表（`028_apps.sql` 编号永久留空）、`/v1/apps*`、`AppRepository`/`AppInstaller`、提示词 App 摘要。UI「MCP」改称「连接器」，底层 `ext_type` 仍为 `mcp`。
- 顶层 `ext_type` 保留：`skill` / `mcp` / `plugin` / `automation` / `agent`。

## 决策二：插件清单多格式归一化（唯一内部模型 `protocol.PluginManifest`，唯一解析器 `internal/extension/pluginspec`）

| 优先级 | 清单 | 来源 | 备注 |
|---|---|---|---|
| 1 | 根 `plugin.json`（`$schema` 指向 agent-plugins.org） | Agent Plugins 1.0 | `extensions["com.openai"]` 为对象时整体替换 `.codex-plugin/plugin.json`（不合并） |
| 2 | `.claude-plugin/plugin.json` | Anthropic | 可缺省，按默认布局加载 |
| 3 | `.codex-plugin/plugin.json` | OpenAI Codex | `interface` 为展示元数据 |

- 身份字段取最高优先级非空值。组件按「默认位置 + 清单声明」解析：Claude `commands/agents/outputStyles` 替换默认目录、`skills` 追加、`hooks/mcpServers/lspServers` 合并（后声明覆盖）；同一物理路径按绝对路径去重。
- 路径：组件路径须 `./` 开头、解析后在插件根内且存在，含 `..` 拒绝；违规组件单独失败，不连坐。顶层未知字段剥离+告警；严格对象（`userConfig`/`channels`/`lspServers`/`monitors` 项）含未知键 → 该组件失败。
- 路径变量：`${PLUGIN_ROOT}`/`${CLAUDE_PLUGIN_ROOT}` → 安装根；`${PLUGIN_DATA}`/`${CLAUDE_PLUGIN_DATA}` → `<data>/extensions/plugin-data/<id>/`（升级保留、卸载删除）；`${user_config.KEY}`；展开非递归。
- 宿主环境变量：`.mcp.json` 的 `${VAR}`/`${VAR:-default}` 只允许展开 `sanitizeParentEnv` 白名单内变量，其余视为未定义并告警（防插件引用 `${OPENAI_API_KEY}` 注入宿主密钥，R1.15 / HE-7）。
- Polaris 生成物（`PluginCreator`、市场 MCP 包装）用 agent-plugins 1.0 布局。已删除自造格式：`plugin.toml`、`skills.yaml`/`agent-manifest.yaml`、`ai-plugin.json`、`.polaris-plugin/`、`PluginBundleManifest` 私有变体。

## 决策三：组件 → 运行时映射

| 组件 | Polaris 运行时 |
|---|---|
| skills（`skills/<n>/SKILL.md`、清单路径、根 `SKILL.md`） | `skills` 表，`plugin_id` FK；决策五 |
| commands | `user-invocable` 技能，`kind=command` |
| agents（`agents/*.md`） | `lifecycle.AgentDefinitionProvider` → `types.AgentProfileSpec`（来源 `plugin:<id>`，命名 `<plugin>:<agent>`） |
| hooks | `internal/action/hook`；决策六 |
| mcpServers（`.mcp.json`/`mcp.json`/内联/`.mcpb`/`.dxt`） | `mcp_servers`，`plugin_id` FK；类型 `stdio`/`http`/`streamable-http`/`sse`/`ws` |
| userConfig | `plugin_user_config`；`sensitive=true` 经 `credential.Vault` 加密，提示词中替换为占位符 |
| channels / dependencies / defaultEnabled | 见下方补充 |
| apps（`.app.json`） | 决策四 |
| lspServers / outputStyles / themes / monitors / workflows / `bin/` / `settings.json` / `experimental.*` | 解析+校验，不激活，记入 `unsupported_components` 并在 UI 标「此宿主不适用」（`bin/` 不进 PATH） |

**补充：子 Agent 定义与委派**
- 来源：插件 `agents/`（`.md`）；项目 `<root>/.polaris/agents/`、用户 `<data>/agents/`（Claude `.md` + Codex `.toml`，同名 `.md` 优先；项目 > 用户）。私有 `agents/*.yaml` 已删。解析唯一实现 `pluginspec.{ParseAgentFile,ListAgentDir}`。
- 委派入口 `transfer_to_agent` / `list_agents`（内置工具）；`DefaultTaskWorker` 把 `agent_handoff:<name>` 解析为角色规格经 `WithAgentProfile` 注入；未知名称使任务失败并把原因写入 `tasks.result`。
- 角色只收窄能力，在内核执行入口硬拦截（`Agent.checkProfileTool`，可见性过滤仅辅助）：`tools`/`disallowedTools`（Claude 工具名映射 Polaris 内置名；白名单中参数级规则 `Bash(git *)` 无法等价映射 → 整条忽略，黑名单按整个工具拒绝；`Agent(a, b)` 限定委派目标）；只读（Codex `sandbox_mode="read-only"` / Claude `permissionMode: plan`）→ 只放行能力 ≤ `CapReadOnly`，`code_act` 与未知工具 fail-closed，其余模式继承宿主不得放宽；`maxTurns` 收紧 `MaxStepsLimit`；子 Agent 默认不能再委派（受 `SpawnDepth` 门控）。
- 角色指令写入 `ZoneMutableSkill`（`<agent_profile>`），不进 `ZoneImmutable`：用户目录 TaintLow，插件/项目目录 TaintMedium。`skills` 字段预加载，缺失即失败。
- 解析但不生效（记入 `not_applied`）：`model`/`effort`（按阶段池路由，ADR-0105）、`background`、`isolation`、`memory`、`initialPrompt`；插件 agent 的 `hooks`/`mcpServers`/`permissionMode` 按 Claude 安全规则忽略。

**补充：channels**
- 协议按 Claude channels reference：`capabilities.experimental["claude/channel"]` 为对象即声明；事件 `notifications/claude/channel {content, meta}` 渲染为 `<channel source=… k=v>`（meta 键仅 `[A-Za-z0-9_]`）。
- 安装 ≠ 启用：`plugin_channels` 存用户显式开启状态；未启用/未连接/未声明的事件丢弃。启用后事件经 `session.Orchestrator.RunTurn(Headless)` 进专属会话 `ch_plugin_<plugin>_<server>`（同 channel 串行），意图 TaintHigh，回复由模型调用服务器自身回复工具完成。
- 审批转发须另开 `permission_relay` 且服务器声明 `claude/channel/permission`：只接受宿主签发、未过期、同一服务器回传的裁决，经 `GatewayImpl.Respond` 进同一审批守卫；`RiskPrivileged` 不外发。

**补充：dependencies**
- 加载期：依赖须已安装、已启用且满足 npm semver 范围（Masterminds/semver v3）；不满足 → 依赖方安装后置停用并写诊断，启用请求 409，停用/卸载后 `EnforceAll` 迭代到不动点级联停用，启动时在恢复 MCP 连接前执行同一检查。
- `defaultEnabled`：初始启用 = `defaultEnabled` ∧ 依赖满足；用户后续启停不被升级覆盖。
- 自动安装（`marketplace.CatalogInstaller`）：先装缺失依赖（跨市场仅限根市场白名单或用户已装且启用）；版本按 `<name>--v<version>` 标签取最高；循环依赖失败；已装依赖不满足新范围时取全部依赖方范围交集重解析升级，无交集报 `has conflicting version requirements`；自动安装实例标 `auto_dependency`，`POST /v1/plugins/prune` 卸载无依赖方者。

## 决策四：Codex「应用」= 插件内的连接器绑定，不是扩展类型

- `.app.json` = `{"apps":{"<alias>":{"id":"<connector_id>"}}}`，由 `extensions.com.openai.apps` 或 `.codex-plugin/plugin.json` 的 `apps` 指向（默认 `./.app.json`）。
- 安装/升级时 `lifecycle.ResolveAppBindings` 按 `mcp_servers` 的 id/名称/catalog_id 解析：命中 `bound`，未命中（如 `plugin_asdk_app_*`/`connector_*`）`unbound`，UI 引导绑定。落库 `plugin_app_bindings`；不转成 `ext_type=mcp`，不生成 MCP 进程。
- 手动绑定（`lifecycle.BindApp`，`PUT /v1/plugins/{id}/apps/{alias}`）在 `connector_ref` 未变且服务器仍存在时跨升级保留；解析失败不阻断安装。

## 决策五：技能遵循 agentskills.io + 两家扩展字段

- 校验两级（只有两家都拒绝的才算硬错误）：硬错误不加载——frontmatter 非合法 YAML；名称为空、>64 字符、含空白/路径分隔符/`..`/控制字符。规范告警仍加载并在 UI 展示——`name` 非 `[a-z0-9-]`/首尾 `-`/含 `--`/与目录名不一致；`description` 缺失（回落正文首个非空行）或 >1024；`compatibility` >500；`metadata` 非 string→string。
- `name` 缺省取目录名；插件内技能对外名 `<plugin>:<name>`（已带前缀不重复）。Polaris 私有参数（`exec_mode`、`risk_level`、`sandbox` 等）只从 `metadata` 的 `polaris-` 前缀键读取，生成的 SKILL.md 同样如此。
- 全量解析：`license`/`compatibility`/`metadata`/`allowed-tools`；Anthropic 扩展 `when_to_use`/`argument-hint`/`arguments`/`disable-model-invocation`/`user-invocable`/`disallowed-tools`/`model`/`effort`/`context`/`agent`/`paths`；Codex `agents/openai.yaml`（`interface`、`policy.allow_implicit_invocation`、`dependencies.tools`）。`disable-model-invocation: true` 或 `allow_implicit_invocation: false` → 不进模型可见索引；`user-invocable: false` → 仅模型可调。
- 正文变量：`$ARGUMENTS`/`$N`/`$name`、`${CLAUDE_SKILL_DIR|PLUGIN_ROOT|PLUGIN_DATA|SESSION_ID}`。`` !`cmd` ``/` ```! ` 动态注入经统一沙箱（`CallerType="skill_inject"`）受 PolicyGate 约束，插件技能的注入同受「安装 ≠ 信任」（决策六）。`allowed-tools` 只是预授权提示，不绕过 Cedar/PolicyGate（HE-7）。
- `context: fork` + `agent`：模型调用时内核把渲染后的正文经 `transfer_to_agent` 委派给 `agent`（缺省 `general-purpose`），复用异步挂起/恢复与 `SpawnDepth`，恢复分支不重复渲染；用户调用时在子 Agent 内运行、无会话历史；不能再委派的子 Agent 内按普通技能内联。内置类型：Claude `Explore`/`Plan`（只读）、Codex `default`/`worker`/`explorer`。
- agentskills 标准适配器位于 `internal/extension/skill/`，信任用 TrustTier（决策九）。

## 决策六：Hooks 统一为两家共同的 `hooks.json` 模型

- 格式 `{"hooks":{"<Event>":[{"matcher":"<regex>","hooks":[{"type":...}]}]}}`；处理器 `command`（含 `args`、`async`、`timeout`、`statusMessage`）、`http`、`mcp_tool`；`prompt`/`agent` 类型走 LLM 路由（ADR-0105）。
- 事件（`internal/action/hook/hook.go` 17 个）：`SessionStart`/`SessionEnd`/`UserPromptSubmit`/`PreToolUse`/`PermissionRequest`/`PostToolUse`/`PostToolUseFailure`/`SubagentStart`/`SubagentStop`/`Stop`/`StopFailure`/`PreCompact`/`PostCompact`/`Notification`/`Elicitation`/`ElicitationResult`/`Interrupt`。IDE/终端专属事件（`Worktree*`、`CwdChanged`、`FileChanged`、`InstructionsLoaded`、`MessageDisplay` 等）解析不触发。
- 协议兼容两家：stdin JSON 带 `session_id`/`turn_id`/`hook_event_name`/`cwd`/`model`/`permission_mode` 及事件字段；退出码 0/2/其他；JSON 输出识别 `continue`/`stopReason`/`systemMessage`/`decision`/`reason`/`hookSpecificOutput.*`。
- **安装 ≠ 信任**：插件 hooks 按定义内容 hash 记录，未审阅 hash 不执行，定义变更重新待审；用户级 hooks.json 视为已信任，项目级与插件级按哈希信任。
- Hook 输出强制 `TaintLevel=High`，经 PolicyGate 决定注入，只能进 MutableSkill Zone，禁止进 Immutable Zone。
- 执行约束：命令处理器经 `sandbox.RunStdio`（Rust 沙箱封装 argv，与 MCP stdio 同能力），封装失败拒绝裸执行；PreToolUse/PermissionRequest veto-only（`allow` 不绕过 PolicyGate 与人工审批，`updatedInput` 改写后重跑 PolicyGate，`ask` 无交互通道按拒绝）；PostToolUse 同步，`decision:block` 原因与 `additionalContext` 追加到工具输出并强制 TaintHigh；Stop/SubagentStop `decision:block` 续跑上限 3 次（HE-5），hook 发起的调用不再触发 hook；SubagentStart/SubagentStop 由 `orchestrator.SubagentRunner`（委派、fork 技能、agent hook 共用的唯一子 Agent 执行实现）触发，SubagentStart 输出以 `<hook-context>` 附加到子任务。
- `agent` 类型处理器：只读子 Agent（`Read`/`Grep`/`Glob`/`LS`，≤50 轮，禁委派），多轮由内核 FSM 驱动（HE-5），其工具调用以 `protocol.CtxHooksSuppressedKey` 标记不再触发 hooks 与 Subagent 事件。

## 决策七：市场格式与来源

- 读 `.claude-plugin/marketplace.json` 与 `.agents/plugins/marketplace.json`（Codex；`.claude-plugin/marketplace.json` 亦为其旧位置），`pluginspec.GetMarketplace` 合并（同名以 Claude 为主、补 Codex policy/category）。来源：相对路径（含 `metadata.pluginRoot`）、`local`、`github`、`url`、`git-subdir`、`npm`（禁安装脚本）、`archive`（HTTPS + `sha256`）；出站全走 SafeDialer（XR-06）；git 只接受 https 且禁 file 协议。
- **不支持 `command` 来源与 `headersHelper`**：在宿主执行市场声明的任意命令越过安装安全门（HE-2/HE-7），标记不可安装并展示原因。
- Claude `strict` 合并语义（`pluginspec.Load(LoadOptions.Entry)`；`strict:false` 且条目带组件即冲突）、Codex `policy.installation`/`policy.authentication` 按原语义；`INSTALLED_BY_DEFAULT` 只作推荐标记，安装仍由用户触发经 `Manager.InstallExtension`（M13-bis §5.6）。`mcp` 市场用 MCP Registry。
- 用户直给来源安装（`POST /v1/plugins/create`、`/v1/skills/create` 的 `source`，`pluginspec.ParseSourceSpec`）；https `.mcpb` 由安装层经 SafeDialer 预下载；archive 下载带条目 `headers`（路由/身份类头剔除）；添加市场须 `plugin:manage` 授权，添加时同步校验、失败撤销。

## 决策八：MCP 客户端对齐 2026-07-28

- `server/discover` 无状态握手（对 2025-11-25 服务器回退 `initialize`）；`Mcp-Method`/`Mcp-Name` 头；`resultType` 与 `InputRequiredResult` 多轮（MRTR/elicitation，网关 broker + hooks 优先程序化作答）；Tasks 扩展轮询；工具列表变更（旧纪元通知 + `subscriptions/listen`）；OAuth（CIMD、`iss` 校验、按授权服务器绑定凭据）；MCP Apps（`ui://`、`text/html;profile=mcp-app`）在 Web 聊天沙箱 iframe 渲染，UI 发起调用走同一 ExecuteTool 审计路径。逐项见 M13-bis §8.5。
- `sse` 类型的 url 即 SSE 端点本身，不得再拼 `/sse`。
- MCP Apps 沙箱异源用独立监听端口（默认主端口 +1；沙箱需 `allow-same-origin`）；反向代理部署用 `apps_sandbox_origin` / `apps_host_origin` 显式配置。ChatGPT `openai/*` 与 `text/html+skybridge` 作别名归一到标准表示，ChatGPT 专属扩展不定义。
- CIMD 仅在网关以 https 非回环地址对外可达时启用，否则走 DCR 或预注册；OAuth 流程状态（state/code_verifier）只驻内存 10 分钟。
- 主密钥轮换覆盖全部 Vault 密文（OAuth 令牌、客户端密钥、预注册密钥）与插件敏感配置，单事务改写（`repo.RekeyVaultCiphertexts`）；新增 Vault 密文列必须登记其清单。

## 决策九：统一信任模型——五级 TrustTier（原 ADR-0016 决策一，§2.1）

- `TrustSystem(4)` 内置硬编码 → `TrustOfficial(3)` 官方 Publisher 白名单（等同系统权限，`approval=auto`）→ `TrustCommunity(2)` cosign 签名未入白名单 → `TrustLocal(1)` HMAC 本地签名 → `TrustUntrusted(0)` fail-closed 拒绝。替代 `SignatureValid bool`（`pkg/types/enums_security.go`）。
- 官方 Publisher 白名单 `configs/extensions/trusted-publishers.yaml`（内嵌，`config.ListTrustedPublishers` 加载；现含 anthropic/openai/google/modelcontextprotocol/github/microsoft/figma/polarisagi），离线校验为主（Publisher ID + content hash pinning），版本锁定 commit hash + sha256，不自动升级。
- Plugin Catalog 归 M13；`trust_tier` 列内嵌于 `008/015/019/020/021` 原始 DDL。

## 决策十：安装实例表与升级（原 ADR-0016 决策三/四）

- `extension_instances`（`020`）是所有已安装扩展的单一事实来源（`ext_type`/`origin`/`catalog_id`/`runtime_id`/`install_path`/`status`/`installed_version`）；安装状态归实例表（Layer 1），运行参数在 `mcp_servers`/`skills`/`plugins`（Layer 2），插件内子组件不污染基础表。
- 版本：`extension_catalog.version`（市场最新）+ `extension_instances.installed_version`；`POST /v1/plugins/{id}/upgrade`（`HandleUpgradePlugin`）增量升级，保留 `extension_instances.id` 与 `install_path`，不触发卸载副作用。

## 决策十一：MCP 传输层与污点解码（原 ADR-0017 决策一/二）

- Streamable HTTP 为默认远程传输（2025-11-25 spec），SSE 仅兼容旧 server 且标 legacy，stdio 仅本地子进程；新接入 server 必须走 Streamable HTTP（`http`/`streamable-http`/`streamable_http` 归一）。
- MCP JSON-RPC 动态结构禁止 `encoding/json` 直解到 `map[string]interface{}`（丢失 M11 §2.1 TaintLevel）；用 `TaintPreservingDecoder`（`internal/extension/mcp/taint_decoder.go`）递归包装所有 string 叶子为 `TaintedString{Source=MCP, Origin=server_name}`：白名单 server → TaintMedium，其余 TaintHigh。客户端字段读取走 `json.Unmarshal` 抄近路一律驳回。

## 决策十二：skill 子包不设并行本地接口（原 ADR-0002，R1.4）

`internal/extension/skill` 统一使用 `protocol.SkillRegistry`/`protocol.SkillMeta`（`internal/protocol/interfaces_skill.go`），不设与之并行的本地接口/类型（曾致字段语义损失）。真有不兼容需求先扩展 protocol 定义，不重引入本地并行接口。

## 决策十三：原 ADR-0016 决策二残余

- CSV Batch Fan-out（`orchestrator.CSVFanoutJob`，CLI `cli_fanout.go`）状态变更走 EventLog（HE-6），不建独立 SQLite。
- 搁置（P3）：prefix_rule DSL（Cedar 已覆盖）、Permission Profile（OS 沙箱工作量大）。

## 反例守护 / 已驳方案

- 恢复独立 `app` 类型或 `apps` 表 — App 在两家标准中是连接器/插件子组件。
- `.app.json` 条目转 `ext_type=mcp` — 违背 Codex 绑定语义（2026-09-26 用户裁决），会凭空造出无端点的 MCP。
- 保留 App 顶层类型对应 Apps SDK — 富 UI 已是 MCP Apps（服务器能力）。
- 新增 Polaris 私有清单格式/字段变体 — 私有数据只进 `extensions["ai.polarisagi"]`。
- 「安装即信任」执行插件 hooks / 技能注入；`allowed-tools` 旁路 PolicyGate；支持市场 `command` 来源。
- 并存 `SignatureValid bool` 信任；在线 cosign Rekor 作主路径（Tier-0 离线不可用）；按类型分四个安装表；Hook 直写 Immutable Zone；对 LSP/主题实现宿主适配。
- 新 MCP server 用 SSE「以简化」。

## 重新评估触发条件

1. agent-plugins.org 发布 1.1+ 且 Anthropic 采纳为 Claude 主格式 → 重议决策二优先级表。
2. OpenAI 发布可自托管解析的连接器目录 API → 决策四 `unbound` 可自动解析。
3. 新增编辑器/终端宿主形态 → 重议 LSP/outputStyles 等「不适用」组件。
4. Cedar 被证明有真实缺口 → 重议 P3 搁置项。
5. MCP spec 发布新的强制传输要求 → 重议跟进，SSE 仅 legacy 定位不变。

## 沿革

| 日期 | 变更 |
|---|---|
| 2026-05-16 | 原 ADR-0002：skill 子包本地接口消除（决策十二） |
| 2026-05-21/22 | 原 ADR-0016 决策一（五级 TrustTier，取代 ADR-0015 §2.1 中间方案）、决策二（Codex 特性集成）、决策三（实例表，原 ADR-0019）；原 ADR-0017 决策一/二（传输、TaintPreservingDecoder，原 ADR-0018） |
| 2026-07-23 | 原 ADR-0016 决策四：升级版本化（原 ADR-0075） |
| 2026-09-26 | 本 ADR 初稿，修订 0016 决策二（Hook 5 事件 → 两家共同事件模型；Custom Agent YAML → `.md`/`.toml`）与决策三（删 `apps` 表）；决策五校验由「违规即不加载」改为硬错误/规范告警两级（Claude 允许 `name`/`description` 缺省）；子 Agent、`context: fork`、`agent` 型 hook 落地 |
| 2026-09-27 | channels、dependencies（含自动安装/prune）、应用绑定、标准市场同步与来源直装落地 |
| 2026-09-29 | 决策八：MCP 2026-07-28 客户端落地 |
| 2026-10-12 | 并入 ADR-0016/0017 决策一二/0002；0017 决策三（入站 A2A 端点实为私有协议，且「出站不复用 MCP 客户端」被 ADR-0084 推翻）归 ADR-0111；白名单订正为 YAML 配置 |
| 2026-10-17 | 私有命名空间 `extensions["ai.polarisagi"]` 新增 `setupTool` / `statusTool`（插件内裸工具名）：插件自带的一次性设置向导无法靠聊天回合触发（需要模型参与、计 token、结果不确定）。字段仍只进私有命名空间，不触碰两家标准字段；执行走 `ExecuteAppTool` 同一执行体，不新增旁路。接口与解析见 M13-bis §5.3.3 |

## 引用代码

`internal/protocol/{extensions,interfaces_skill}.go`、`internal/extension/{marketplace,lifecycle,native,skill,plugin,pluginspec,mcp}/`、`internal/action/hook/`、`internal/agent/{agent_profile,agent_skill_fork}.go`、`internal/tool/catalog/tool_restriction.go`、`internal/tool/builtin/{list_agents,delegation_tools}.go`、`internal/execute/orchestrator/{subagent_runner,csv_fanout}.go`、`internal/security/{credential/vault,taint/taint}.go`、`internal/config/trusted_publishers.go`、`configs/extensions/trusted-publishers.yaml`、`pkg/types/enums_security.go`、`internal/gateway/elicitation/`、`internal/gateway/server/plugin/`、`internal/store/repo/repo_vault_rekey.go`、`internal/protocol/schema/{008,013,015,018,019,020,021,040}_*.sql`、`docs/arch/M13-bis-Extension-Registry.md`
