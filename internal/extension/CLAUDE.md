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
- [标杆] `native/extension_manager.go`: Manager (统一安装入口)
- [参照] `marketplace/adapter.go`: 多厂商清单解析 (OpenAI/Anthropic/Google→RegistryEntry)
- [参照] `marketplace/loader.go`: Polaris 原生格式解析
- [参照] `marketplace/manager.go`: 市场同步 + 安装协调
- [参照] `mcp/mcp_manager.go`: MCP 进程连接管理
- [参照] `mcp/env.go`: sanitizeParentEnv (MCP 子进程环境净化)
- [参照] `skill/skill_creator.go`: Skill 规范解析 + Wasm 委托
- [参照] `native/builtin/`: builtin 内置工具 (install_extension / search_extension)

**跨模块**:
- 信任策略由 pkg/substrate/policy 决定, 本包仅传递 trust_tier
- MCP 进程生命周期由本包 MCPManager 管理; Wasm 执行委托 pkg/action WazeroRuntime
