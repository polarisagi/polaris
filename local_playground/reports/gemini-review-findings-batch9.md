# Polaris 代码轨道 批次 9 审核报告

## 本批次概览

- 批次: 9 (9.1 server 核心 middleware/logstream/根级 + 9.2 server 子包 chat/plugin/provider/sysadmin/egress/authcontext/types)
- 覆盖层: L3 网关层
- 侧重维度: 9.1: B, D, G; 9.2: B, D, E
- 结果: 共 3 条发现 (P0: 1, P1: 1, P2: 1)
- 条目 ID 区间: GR-9-001 ~ GR-9-003

置信度分布声明: 本批次全部条目置信度均填「高」，因为已按照 §2-A 执行反证并给出了物理可达的代码事实与行号，无需假定运行时条件。

| ID | 严重级 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-9-001 | P1 | internal/gateway/server | handleAgentInterrupt 未将 WebUI 默认客户端类型纳入 whitelist 导致中断请求遭 403 拒绝 | 高 | 是 |
| GR-9-002 | P0 | internal/gateway/authcontext | ContextRefExpander.resolveFile 未校验 workDir 隔离边界导致任意文件读取 | 高 | 是 |
| GR-9-003 | P2 | internal/gateway/server | HandleTogglePluginMCP 路由与 handler 路径参数不匹配导致永远 400 | 高 | 是 |

---

### [GR-9-001] handleAgentInterrupt 未将 WebUI 默认客户端类型纳入 whitelist 导致中断请求遭 403 拒绝
- 严重级: P1
- 模块: internal/gateway/server（层: L3）
- 位置: internal/gateway/server/server_handlers_hitl.go:131
- 违反规则: HE-7
- 置信度: 高
- 可机械化: 是（建议规则: 检查 handleAgentInterrupt 中 authCtx.ClientType 的判断，比对 middleware_auth.go 注入的 WebUI 客户端类型 webui）
- 反证: 已核对 cmd/polaris/boot_server.go、internal/bootstrap/、middleware_auth.go 与 server_handlers_hitl.go。在未配置 POLARIS_API_KEY 的开发/本地部署场景下，middleware_auth.go:68 为 loopback 请求注入的 ClientType 为 "webui" 且 Authenticated 为 false。然而 handleAgentInterrupt (server_handlers_hitl.go:131) 在检查权限时仅判断 authCtx.ClientType == "local_webui" || authCtx.ClientType == "local"，并不匹配 "webui"。因 Authenticated 为 false 且 ClientType 不匹配，isLocalWebUI 为 false，所有来自本地 WebUI 的任务中断请求 (POST /v1/agent/{taskID}/interrupt) 均必定被 403 拒绝。
- 问题: 当用户在 WebUI 中点击“中止/恢复/重定向”Agent 任务时，`POST /v1/agent/{taskID}/interrupt` 端点会被调用。`middleware_auth.go` 在零认证配置下将本机请求的 `ClientType` 设为 `"webui"`，但 `handleAgentInterrupt` 中的硬编码判断只检查了 `"local_webui"` 和 `"local"`，未能包含 `"webui"`，导致用户合法的中断操作必定触发 `403 Forbidden: unauthorized user`，无法中断运行中的 Agent。
- 证据: 关键代码摘录如下
  ```go
  // middleware_auth.go:68
  return authcontext.WithAuthContext(ctx, &authcontext.AuthContext{UserID: "anonymous", ClientType: "webui", TraceID: traceID, Authenticated: false}), true

  // server_handlers_hitl.go:130-135
  isAdmin := authCtx.Authenticated && (authCtx.UserID == "admin" || authCtx.UserID == "system")
  isLocalWebUI := authCtx.ClientType == "local_webui" || authCtx.ClientType == "local"
  if !isAdmin && !isLocalWebUI {
      http.Error(w, "forbidden: unauthorized user", http.StatusForbidden)
      return
  }
  ```
- 修复方向提示: 在 `server_handlers_hitl.go:131` 的 `isLocalWebUI` 判断条件中增加 `authCtx.ClientType == "webui"`。

### [GR-9-002] ContextRefExpander.resolveFile 未校验 workDir 隔离边界导致任意文件读取
- 严重级: P0
- 模块: internal/gateway/authcontext（层: L3）
- 位置: internal/gateway/authcontext/contextref.go:239-251
- 违反规则: HE-7
- 置信度: 高
- 可机械化: 是（建议规则: 扫描 AST 中 os.ReadFile 前的 filepath 校验逻辑，确保存在 strings.HasPrefix(abs, cleanWorkDir) 越界断言）
- 反证: 已核对 server_lifecycle.go:160（通过 WithWorkDir(s.dataDir) 初始化 ContextRefExpander）与 authcontext/contextref.go:239-251。当用户输入中包含 `@file:"/etc/passwd"` 或带 `..` 相对路径（如 `@file:"../../../../etc/passwd"`）时，resolveFile 的 `!filepath.IsAbs(path)` 判断直接跳过 workDir 拼接；后续仅使用 `isSensitivePath(abs)` 检查黑名单（仅拦截 `.ssh` / `.aws` 等硬编码路径），完全没有检查 `abs` 是否位于 `e.workDir` 目录之内。攻击者或恶意 prompt 只要构造绝对路径，即可绕过工作区限制读取宿主机任意文件（在 1MB 上限内）并拼接入 LLM 上下文。
- 问题: `ContextRefExpander.resolveFile` 负责解析用户消息中的 `@file` 标签并读取文件内容追加到对话上下文。然而在解析绝对路径或包含 `..` 的路径时，未校验解析出的绝对路径 `abs` 是否属于授权的 `workDir` 根目录。`isSensitivePath` 仅维护了一个有限的黑名单（如 `.ssh`, `.aws`），未包含 `/etc/passwd`、系统配置文件、Polaris 数据库等大量敏感目标。这使得攻击者可以通过发送包含 `@file` 绝对路径的消息，在未授权情况下读取服务器任意文件。
- 证据: 关键代码摘录如下
  ```go
  // authcontext/contextref.go:239-251
  if !filepath.IsAbs(path) && e.workDir != "" {
      path = filepath.Join(e.workDir, path)
  }

  abs, err := filepath.Abs(path)
  if err != nil {
      return "", 0, apperr.Wrap(apperr.CodeInternal, "resolve path", err)
  }
  if isSensitivePath(abs) {
      return "", 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("blocked: sensitive path %q", abs))
  }

  data, err := os.ReadFile(abs)
  ```
- 修复方向提示: 在 `resolveFile` 中校验 `e.workDir` 不为空时，要求 `abs` 必须以 `filepath.Clean(e.workDir) + string(filepath.Separator)` 为前缀，阻止越界读取 `workDir` 之外的文件。

### [GR-9-003] HandleTogglePluginMCP 路由与 handler 路径参数不匹配导致永远 400
- 严重级: P2
- 模块: internal/gateway/server（层: L3）
- 位置: internal/gateway/server/server_routes.go:203
- 违反规则: HE-7
- 置信度: 高
- 可机械化: 是（建议规则: 提取 mux.HandleFunc 模式中的路径参数与 Handler 中 r.PathValue 调用的 key 集合，校验子集包含关系）
- 反证: 已核对 server_routes.go:203 与 internal/gateway/server/plugin/manage.go:279-282。`server_routes.go:203` 将 `POST /v1/plugins/{id}/toggle` 绑定到 `HandleTogglePluginMCP`。但 `HandleTogglePluginMCP` 内部试图通过 `r.PathValue("serverName")` 读取路径参数 `serverName`。由于注册的路由中根本不包含 `{serverName}` 占位符，`r.PathValue("serverName")` 必定返回 `""`，从而触发 line 280 的 `if pluginID == "" || serverName == ""` 校验，强制返回 `400 Bad Request: id and serverName required`。此 HTTP 端点在生产环境中 100% 无法正常工作。
- 问题: 在 `server_routes.go` 中注册插件子 MCP 切换路由时，填写的路由模式为 `POST /v1/plugins/{id}/toggle`；而在 `HandleTogglePluginMCP` 的实现中，要求从 URL 路径参数中读取 `id` 与 `serverName`。由于路由定义遗漏了 `{serverName}` 占位符（正确格式应为 `POST /v1/plugins/{id}/mcp/{serverName}/toggle` 或在 request body 中传递 `serverName`），handler 提取出的 `serverName` 始终为空字符串，导致该 API 任何请求均直接触发 400 校验错误。
- 证据: 关键代码摘录如下
  ```go
  // server_routes.go:203
  mux.HandleFunc("POST /v1/plugins/{id}/toggle", s.pluginHandler.HandleTogglePluginMCP)

  // plugin/manage.go:278-283
  pluginID := r.PathValue("id")
  serverName := r.PathValue("serverName")
  if pluginID == "" || serverName == "" {
      http.Error(w, "id and serverName required", http.StatusBadRequest)
      return
  }
  ```
- 修复方向提示: 将 `server_routes.go:203` 的路由修改为 `POST /v1/plugins/{id}/mcp/{serverName}/toggle`（或 `PATCH /v1/plugins/{id}/mcp/{serverName}`）以匹配 handler 的参数提取模式。

---

## 已审文件清单

- internal/gateway/authcontext/context.go
- internal/gateway/authcontext/contextref.go
- internal/gateway/egress/egress_gateway.go
- internal/gateway/server/handler_codeact.go
- internal/gateway/server/logstream.go
- internal/gateway/server/middleware.go
- internal/gateway/server/middleware_auth.go
- internal/gateway/server/provider.go
- internal/gateway/server/server_core.go
- internal/gateway/server/server_handlers.go
- internal/gateway/server/server_handlers_hitl.go
- internal/gateway/server/server_init.go
- internal/gateway/server/server_lifecycle.go
- internal/gateway/server/server_routes.go
- internal/gateway/server/server_setters_eval.go
- internal/gateway/server/server_setters_modelregistry.go
- internal/gateway/server/server_setters_sampling.go
- internal/gateway/server/server_setters_steering.go
- internal/gateway/server/chat/audio_service.go
- internal/gateway/server/chat/chat_message_persist_handler.go
- internal/gateway/server/chat/compression_service.go
- internal/gateway/server/chat/compressor_helpers.go
- internal/gateway/server/chat/handler.go
- internal/gateway/server/chat/persistence_service.go
- internal/gateway/server/chat/prompt_assembly_service.go
- internal/gateway/server/chat/provider.go
- internal/gateway/server/chat/recap.go
- internal/gateway/server/chat/sessions.go
- internal/gateway/server/chat/sessions_helpers.go
- internal/gateway/server/chat/sessions_search.go
- internal/gateway/server/chat/slash_command_steer.go
- internal/gateway/server/chat/slash_commands.go
- internal/gateway/server/chat/sse.go
- internal/gateway/server/chat/sse_sink.go
- internal/gateway/server/chat/sse_stream_helpers.go
- internal/gateway/server/chat/sse_transport.go
- internal/gateway/server/chat/system_prompt.go
- internal/gateway/server/chat/system_prompt_ambient.go
- internal/gateway/server/chat/system_prompt_extensions.go
- internal/gateway/server/plugin/catalog.go
- internal/gateway/server/plugin/catalog_download.go
- internal/gateway/server/plugin/catalog_handlers.go
- internal/gateway/server/plugin/catalog_install.go
- internal/gateway/server/plugin/catalog_register.go
- internal/gateway/server/plugin/custom.go
- internal/gateway/server/plugin/custom_app_mcp.go
- internal/gateway/server/plugin/custom_plugin_intent.go
- internal/gateway/server/plugin/embedding_indexer.go
- internal/gateway/server/plugin/handler.go
- internal/gateway/server/plugin/manage.go
- internal/gateway/server/plugin/manage_upgrade.go
- internal/gateway/server/plugin/provider.go
- internal/gateway/server/plugin/sync.go
- internal/gateway/server/plugin/sync_parsers.go
- internal/gateway/server/plugin/sync_parsers_ecosystem.go
- internal/gateway/server/provider/catalog.go
- internal/gateway/server/provider/handler.go
- internal/gateway/server/provider/provider.go
- internal/gateway/server/provider/providers.go
- internal/gateway/server/provider/providers_models.go
- internal/gateway/server/provider/providers_models_registry.go
- internal/gateway/server/provider/providers_probe.go
- internal/gateway/server/provider/seed.go
- internal/gateway/server/sysadmin/a2a/admin_a2a.go
- internal/gateway/server/sysadmin/admin_killswitch.go
- internal/gateway/server/sysadmin/agent_profile.go
- internal/gateway/server/sysadmin/apps.go
- internal/gateway/server/sysadmin/budget.go
- internal/gateway/server/sysadmin/channelsadmin/admin.go
- internal/gateway/server/sysadmin/channelsadmin/channels_crud.go
- internal/gateway/server/sysadmin/channelsadmin/webhook_receive.go
- internal/gateway/server/sysadmin/channelsadmin/webhook_verify.go
- internal/gateway/server/sysadmin/cronadmin/admin.go
- internal/gateway/server/sysadmin/cronadmin/cron_handlers.go
- internal/gateway/server/sysadmin/cronadmin/cron_runner.go
- internal/gateway/server/sysadmin/cronadmin/cron_scheduler.go
- internal/gateway/server/sysadmin/cronadmin/cron_templates_handlers.go
- internal/gateway/server/sysadmin/cronadmin/cron_util.go
- internal/gateway/server/sysadmin/csv_fanout.go
- internal/gateway/server/sysadmin/doctor.go
- internal/gateway/server/sysadmin/evaladmin/admin.go
- internal/gateway/server/sysadmin/export.go
- internal/gateway/server/sysadmin/handler.go
- internal/gateway/server/sysadmin/hooks.go
- internal/gateway/server/sysadmin/insightsadmin/insights.go
- internal/gateway/server/sysadmin/mcpadmin/admin.go
- internal/gateway/server/sysadmin/mcpadmin/mcp_servers.go
- internal/gateway/server/sysadmin/mcpadmin/mcp_servers_runtime.go
- internal/gateway/server/sysadmin/openai_compat.go
- internal/gateway/server/sysadmin/pattern_dag.go
- internal/gateway/server/sysadmin/pattern_other.go
- internal/gateway/server/sysadmin/pipeline.go
- internal/gateway/server/sysadmin/preferences.go
- internal/gateway/server/sysadmin/prompts.go
- internal/gateway/server/sysadmin/provider.go
- internal/gateway/server/sysadmin/skill_create.go
- internal/gateway/server/sysadmin/system_update.go
- internal/gateway/server/sysadmin/tools.go
- internal/gateway/server/sysadmin/vfs.go
- internal/gateway/server/sysadmin/workflowadmin/admin.go
- internal/gateway/server/sysadmin/workflowadmin/workflow.go
- internal/gateway/server/sysadmin/workflowadmin/workflow_cron.go
- internal/gateway/server/sysadmin/workflowadmin/workflow_engine.go
- internal/gateway/server/sysadmin/workflowadmin/workflow_graph.go
- internal/gateway/server/sysadmin/workflowadmin/workflow_handlers.go
- internal/gateway/server/sysadmin/workflowadmin/workflow_step_worker.go
- internal/gateway/types/compressor.go
- internal/gateway/types/mcp.go

## 明确未覆盖的范围

无

## 审了但无发现的模块

- internal/gateway/server/logstream.go
- internal/gateway/server/provider.go
- internal/gateway/server/server_core.go
- internal/gateway/server/server_init.go
- internal/gateway/server/server_lifecycle.go
- internal/gateway/server/sysadmin
