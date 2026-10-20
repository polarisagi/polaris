# internal/extension — 扩展层（L2：市场/安装/路由）

> 架构文档：[M13-bis-Extension-Registry.md](../../docs/arch/M13-bis-Extension-Registry.md)；决策：ADR-0103（扩展体系）

**硬约束**
1. 安全门强制：`marketplace/manager.go` 的 `Manager.InstallExtension` 是唯一安装入口，nil → 503 + return，禁静默跳过（R1.14）
2. MCP 子进程必经 `sanitizeParentEnv()`（`mcp/env.go`）过滤 `*_KEY/_TOKEN/_SECRET`，禁 `cmd.Env = os.Environ()`（R1.15，门控 `Test_inv_MCPSubprocessEnvSanitized`）
3. Bundle 内子 MCP 逐个独立过 PolicyGate，失败 skip + Warn，不中断父
4. 出站网络：禁裸 `http.Client`，全走 `security/network` SafeDialer（XR-06）
5. 文件系统操作：经 `internal/sandbox` 提供的安全接口（XR-11）
6. 依赖单向：不得 import L3（gateway/automation/eval/channel/sysmgr），门控 `Test_inv_NoCrossLayerImport`

**高频陷阱**
- `extension_instances`（020）是安装状态 SSoT；写入前必经 `Manager.InstallExtension`
- 禁直接 INSERT `mcp_servers`/`skills`/`plugins`，必由安装层绑定写入
- `trust_tier` 由 `security/policy` 决定，本包原样传递不做策略判定
- 工具懒加载阈值 40（`lazy_load_tool_threshold`）：超限仅暴露静态核心集（含 `tool_search`），其余由系统提示索引 + `tool_search` 激活（ADR-0119，不依赖 Embedder）
- ambient skill 注入上限 4000 字符（`ambient_skill_max_chars`）；超限按 trust_tier 降序截断

**文件索引**
- [**总入口**] `marketplace/manager.go`：`Manager.InstallExtension`（唯一安装网关）+ `CompleteInstall` → `lifecycle.InstallFSM` 分发。无 `bus/` `facade` 层，AI 查代码勿再找
- [**唯一解析器**] `pluginspec/`：插件（agent-plugins 1.0 / .claude-plugin / .codex-plugin）、技能（agentskills.io + 两家扩展）、MCP 配置的唯一解析实现（ADR-0103 决策二）；安装器与 gateway 不得另写清单解析
- [**安装器**] `lifecycle/{plugin,skill,mcp}_installer.go`；MCP 连接一律 `MCPManager.StartFromDB`（`mcp_servers` 行是配置权威源）
- [**市场**] `marketplace/catalog_{sync,install,versions,prune}.go` + `sources.go` + `source_npm.go` + `mcp_registry.go`：标准市场目录同步、来源取回（https/SafeDialer，禁 command 来源）、依赖先装（ADR-0103 决策七）
- [参照] `mcp/mcp_manager*.go`（MCP 进程连接管理）、`mcp/taint_decoder.go`（TaintPreservingDecoder）、`native/extension_manager.go` + `native/builtin/`（install_extension / search_extension 内置工具）、`skill/`（注册、选择、执行、skill_creator）、`plugin/plugin_creator.go`、`llmgen/`

**跨模块**
- 信任策略由 `internal/security/policy` 决定，本包仅传递 trust_tier
- MCP 进程生命周期由本包 `MCPManager` 管理；Skill 执行经 `internal/sandbox`（ContainerSandbox L3）
