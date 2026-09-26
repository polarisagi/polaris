# pkg/extensions/ (L2 扩展层: M13-bis 市场/安装/路由)

> Canonical arch doc: [M13-bis-Extension-Registry.md](../../docs/arch/M13-bis-Extension-Registry.md)

**硬约束**:
1. 安全门强制: InstallExtension 是唯一安装入口, nil→503+return, 禁静默跳过 (R1.14)
2. MCP 子进程: 必 sanitizeParentEnv() 过滤 `*_KEY/_TOKEN/_SECRET`, 禁 `cmd.Env = os.Environ()` (R1.15)
3. Bundle 子 MCP: installBundleMCP 内部独立调用 PolicyGate, 失败 skip+Warn 不中断父
4. 出站网络: 禁裸 http.Client, 全部走 M11 SafeDialer (XR-06)
5. 文件系统操作: 调度 pkg/action/sandbox 提供安全接口 (XR-11)
6. 依赖单向: 禁 import pkg/{governance,edge,gateway}

**高频陷阱**:
- extension_instances 是安装状态 SSoT; 写入前必经 Manager.InstallExtension
- 禁直接 INSERT mcp_servers/skills/plugins, 必由安装层绑定写入
- trust_tier 由 M11 决定, 本包原样传递不做策略判定
- 工具懒加载阈值 40: 超限仅暴露 builtin(trust_tier=4) + search_tools
- ambient skill 注入上限 4000 字符; 超限按 trust_tier 降序截断

**文件索引**:
- [**总入口**] `marketplace/manager.go`: `Manager.InstallExtension`（唯一安装网关，见上方硬约束1；
  2026-07-13 deadcode 复核确认 `bus/bus.go` 的 `ExtensionBus`/`ExtensionFacade` 从未被任何调用方
  使用——全部真实调用点均直接走本文件，与本文档§硬约束1、`M13-bis-Extension-Registry.md §6`
  描述一致——已删除，AI 查代码请以此为准，不要再找 bus/facade）
- [标杆] `native/extension_manager.go`: 原生工具 (InstallExtensionFn)
- [**唯一解析器**] `pluginspec/`: 插件（agent-plugins 1.0 / .claude-plugin / .codex-plugin）、技能（agentskills.io
  + 两家扩展）、MCP 配置的唯一解析实现（ADR-0103 决策二）；安装器与 gateway 不得另写清单解析。
- [**安装器**] `lifecycle/{plugin,skill,mcp}_installer.go`: 经 `Manager.CompleteInstall` → `InstallFSM` 分发；
  MCP 连接一律 `MCPManager.StartFromDB`（行是配置权威源）。
- [**市场**] `marketplace/catalog_{sync,install,versions}.go` + `sources.go` + `source_npm.go` + `mcp_registry.go`:
  标准市场目录同步、来源取回（https/SafeDialer，禁 command 来源）与依赖先装（ADR-0103 决策七）；
  旧 `adapter*.go` 启发式爬虫与 `catalog.json` 私有格式已删除（2026-09-27）。
- [参照] `marketplace/manager.go`: 市场同步 + 安装协调
- [参照] `mcp/mcp_manager.go`: MCP 进程连接管理
- [参照] `mcp/env.go`: sanitizeParentEnv (MCP 子进程环境净化)
- [参照] `skill/skill_creator.go`: Skill 规范解析 + Wasm 委托
- [参照] `native/builtin/`: builtin 内置工具 (install_extension / search_extension)

**跨模块**:
- 信任策略由 pkg/substrate/policy 决定, 本包仅传递 trust_tier
- MCP 进程生命周期由本包 MCPManager 管理; Wasm 执行委托 pkg/action WazeroRuntime
