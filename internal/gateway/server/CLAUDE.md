# internal/gateway/server — AI 导航索引

> 背景：本目录文件多（含测试 200+）。Go 的 `func (s *Server) ...` 方法绑定使顶层处理函数无法拆独立子包，故**路由处理**按功能域放入子包，**核心基础设施**留顶层。文件以实际目录为准，`ls` 优先于本表。

## 顶层文件（核心基础设施）

| 文件 | 职责 |
|------|------|
| `server_core.go` / `server_lifecycle.go` / `server_init.go` / `server_wiring.go` | Server 定义、生命周期、依赖注入与装配 |
| `server_routes.go` / `server_handlers*.go` | 路由注册入口、顶层处理函数（elicitation / hitl / mcp_apps 等分文件） |
| `server_mesh*.go` | 跨节点（mesh）设置与管理路由（ADR-0111） |
| `server_setters_*.go` | eval / modelregistry / sampling / steering 的后置 setter 注入 |
| `middleware.go` / `middleware_auth.go` | 认证 / 限流 / 日志等全局中间件链 |
| `logstream.go` | SSE 日志流推送 |
| `oauth_mcp_handlers.go` / `vault_rotate.go` / `handler_codeact.go` / `server_apps_sandbox.go` | MCP OAuth、密钥轮换、CodeAct、MCP Apps 沙箱 |

## 子包

| 子包 | 职责 | 关键文件 |
|------|------|------|
| `chat/` | 对话与会话 | `handler.go` 路由；`sessions*.go` 会话 CRUD/检索；`sse*.go` 流式输出；`audio_*.go` 语音；`compression_service.go` / `compressor_*.go` 长上下文压缩；`recap.go`；`slash_commands*.go` / `slash_skills.go`；`system_prompt*.go` / `prompt_assembly_service.go` |
| `plugin/` | 插件/扩展管理 | `handler.go`；`catalog*.go` 目录与安装；`manage*.go` 安装/卸载/升级；`custom*.go` 自定义；`sync.go`；`agents_/apps_/channels_/config_/deps_/hooks_handlers.go` |
| `provider/` | LLM Provider 管理 | `handler.go`；`providers*.go` CRUD/模型/探测；`catalog.go`；`seed.go` |
| `sysadmin/` | 系统管理 | `handler.go`；`openai_compat.go`（`/v1/chat/completions`）；`budget.go` `usage.go` `doctor.go` `export.go` `vfs.go` `tools.go` `prompts.go` `preferences.go` `system_update.go` `vault_admin.go` `admin_killswitch.go`；子包 `a2a/` `channelsadmin/` `cronadmin/` `evaladmin/` `insightsadmin/` `mcpadmin/` `workflowadmin/` |
| `meshapi/` | 对端节点入口 | `a2a.go` `mcp.go` `card.go`（Agent Card）`principal.go` `routes.go`（ADR-0111） |

## 常用查找

| 场景 | 目标 |
|------|------|
| 修改 SSE 流式输出 | `chat/sse*.go` |
| 新增路由 | 对应子包 `handler.go` + `server_routes.go` 注册 |
| 修改认证逻辑 | `middleware.go` / `middleware_auth.go` |
| 插件安装流程 | `plugin/manage.go`、`plugin/catalog_install.go` |
| OpenAI 兼容接口 | `sysadmin/openai_compat.go` |
| MCP Server 配置 | `sysadmin/mcpadmin/` |
| 定时任务 / 工作流 API | `sysadmin/cronadmin/`、`sysadmin/workflowadmin/` |
| Agent 日志推送 | `logstream.go` |
