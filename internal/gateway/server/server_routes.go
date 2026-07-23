package server

import (
	"net/http"

	"github.com/polarisagi/polaris/internal/observability/metrics"
)

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/doctor", s.sysadminHandler.HandleDoctor)
	mux.Handle("GET /metrics", metrics.MetricsHandler(s.tbr))
	mux.HandleFunc("GET /v1/logs/stream", s.handleLogStream)
	mux.HandleFunc("POST /v1/agent/query", s.handleAgentQuery)
	mux.HandleFunc("POST /v1/agent/codeact", s.handleCodeAct)
	mux.HandleFunc("POST /v1/agent/stream", s.chatHandler.HandleAgentStream)
	mux.HandleFunc("GET /v1/agent/tasks/{taskID}", s.handleGetAgentTask)
	mux.HandleFunc("POST /v1/agent/{taskID}/interrupt", s.handleAgentInterrupt)      // inv_global_08 <200ms
	mux.HandleFunc("GET /v1/agent/mmd-canvas", s.sysadminHandler.HandleGetMMDCanvas) // M05 §11.3 TaskMermaidCanvas 只读展示
	mux.HandleFunc("GET /v1/approvals/pending", s.handleGetPendingApprovals)
	mux.HandleFunc("POST /v1/approvals/", s.handleResolveApproval) // /v1/approvals/{id}/resolve

	// KillSwitch (M11) 管理端点
	mux.HandleFunc("POST /_admin/kill", s.sysadminHandler.HandleKill)
	mux.HandleFunc("POST /_admin/unseal", s.sysadminHandler.HandleUnseal)

	// 厂商字典 API（只读，内置种子）
	mux.HandleFunc("GET /v1/catalog/providers", s.providerHandler.HandleListCatalogProviders)

	// LLM 厂商配置 API
	mux.HandleFunc("GET /v1/providers", s.providerHandler.HandleListProviders)
	mux.HandleFunc("POST /v1/providers", s.providerHandler.HandleCreateProvider)
	mux.HandleFunc("POST /v1/providers/from-catalog", s.providerHandler.HandleCreateProviderFromCatalog)
	mux.HandleFunc("PUT /v1/providers/{providerID}", s.providerHandler.HandleUpdateProvider)
	mux.HandleFunc("DELETE /v1/providers/{providerID}", s.providerHandler.HandleDeleteProvider)
	mux.HandleFunc("POST /v1/providers/{providerID}/test", s.providerHandler.HandleTestProvider)

	// 厂商模型管理 API（两层架构：provider → models）
	mux.HandleFunc("GET /v1/providers/{providerID}/models", s.providerHandler.HandleListModels)
	mux.HandleFunc("POST /v1/providers/{providerID}/models", s.providerHandler.HandleCreateModel)
	mux.HandleFunc("PUT /v1/providers/{providerID}/models/{modelID}", s.providerHandler.HandleUpdateModel)
	mux.HandleFunc("DELETE /v1/providers/{providerID}/models/{modelID}", s.providerHandler.HandleDeleteModel)

	// ModelVersionRegistry 运营触发入口（P3-2，M01 §9；2026-07-21 deadcode 审查补齐）
	mux.HandleFunc("POST /v1/providers/{providerID}/models/{modelID}/upgrade", s.providerHandler.HandleModelUpgrade)
	mux.HandleFunc("POST /v1/providers/{providerID}/models/{modelID}/deprecate", s.providerHandler.HandleModelDeprecate)

	// 模型角色配置 API（对话模型 / 推理模型）
	mux.HandleFunc("GET /v1/config/model-roles", s.providerHandler.HandleGetModelRoles)
	mux.HandleFunc("PUT /v1/config/model-roles", s.providerHandler.HandleSetModelRoles)
	mux.HandleFunc("GET /v1/config", s.handleGetConfig)
	mux.HandleFunc("GET /v1/config/server", s.handleGetConfig)

	// Preferences API
	mux.HandleFunc("GET /v1/preferences", s.sysadminHandler.HandleGetPreferences)
	mux.HandleFunc("PUT /v1/preferences/{key}", s.sysadminHandler.HandleSetPreference)

	// 提示词管理 API（三层所有权：Layer 1 用户自定义层，读写 ~/.polarisagi/polaris/config/prompts/）
	// Layer 0（embedded 内置默认）和 Layer 2（M9 优化）不通过此 API 暴露
	// mux.HandleFunc("...", s.xx.HandleListPromptVersions)
	// mux.HandleFunc("...", s.handleGetPrompt)
	// mux.HandleFunc("...", s.handleSetPrompt)
	// mux.HandleFunc("...", s.handleResetPrompt)

	// M12 评测 API — V8-S2 Meta-Eval Sentinel（meta_holdout 隔离分区）运维接口，
	// 见 internal/gateway/server/sysadmin/evaladmin。均要求 meta_auditor 签名
	// （GET 状态查询除外），签名由运维本地 `polaris eval sign` 离线生成。
	mux.HandleFunc("POST /v1/eval/meta-holdout/cases", s.sysadminHandler.Eval.HandleAddMetaHoldoutCase)
	mux.HandleFunc("POST /v1/eval/meta-audit", s.sysadminHandler.Eval.HandleRunMetaAudit)
	mux.HandleFunc("GET /v1/eval/meta-audit", s.sysadminHandler.Eval.HandleGetMetaAuditStatus)
	// handleEvalRun：boot_server.go 已通过 SetEvalRunner 注入 ab.EvalRunner，但此前
	// 从未注册为路由（server_handlers.go 上带 //nolint:unused 标记），触发 M12
	// 评测套件的 REST 入口一直不可达，只能靠内部代码直接调用 EvalRunner。
	mux.HandleFunc("POST /v1/eval/run", s.handleEvalRun)

	// 会话历史 API
	mux.HandleFunc("GET /v1/sessions", s.chatHandler.HandleListSessions)
	mux.HandleFunc("GET /v1/sessions/{sessionID}", s.chatHandler.HandleGetSession)
	mux.HandleFunc("GET /v1/sessions/{sessionID}/context", s.chatHandler.HandleGetSessionContext)
	// mux.HandleFunc("...", s.xx.HandleGetHistory)
	mux.HandleFunc("DELETE /v1/sessions/{sessionID}", s.chatHandler.HandleDeleteSession)

	// 语音识别 API
	mux.HandleFunc("POST /v1/audio/transcriptions", s.chatHandler.HandleAudioTranscriptions)
	mux.HandleFunc("POST /v1/audio/speech", s.chatHandler.HandleAudioSpeech)

	// VFS 通用文件上传
	// mux.HandleFunc("...", s.xx.HandleUpload)

	// 全文搜索 API（FTS5）
	mux.HandleFunc("GET /v1/search", s.chatHandler.HandleSearch)

	// 用量洞察 & 会话回顾
	mux.HandleFunc("GET /v1/insights", s.sysadminHandler.HandleInsights)
	mux.HandleFunc("POST /v1/sessions/{sessionID}/recap", s.chatHandler.HandleSessionRecap)

	// Trajectory 导出（自演化训练数据）
	mux.HandleFunc("GET /v1/export/trajectories", s.sysadminHandler.HandleExportTrajectories)

	// 自动化 (Automations)
	mux.HandleFunc("GET /v1/automations", s.sysadminHandler.Cron.HandleListAutomations)
	mux.HandleFunc("POST /v1/automations", s.sysadminHandler.Cron.HandleCreateAutomation)
	mux.HandleFunc("PUT /v1/automations/{id}", s.sysadminHandler.Cron.HandleUpdateAutomation)
	mux.HandleFunc("DELETE /v1/automations/{id}", s.sysadminHandler.Cron.HandleDeleteAutomation)
	mux.HandleFunc("GET /v1/automations/{id}/runs", s.sysadminHandler.Cron.HandleListAutomationRuns)
	mux.HandleFunc("POST /v1/automations/{id}/trigger", s.sysadminHandler.Cron.HandleTriggerAutomation)
	mux.HandleFunc("GET /v1/automation-templates", s.sysadminHandler.Cron.HandleListAutomationTemplates)

	// 工作流 (Workflows)
	mux.HandleFunc("GET /v1/workflows", s.sysadminHandler.Workflow.HandleListWorkflows)
	mux.HandleFunc("POST /v1/workflows", s.sysadminHandler.Workflow.HandleCreateWorkflow)
	mux.HandleFunc("GET /v1/workflows/{id}", s.sysadminHandler.Workflow.HandleGetWorkflow)
	mux.HandleFunc("PUT /v1/workflows/{id}", s.sysadminHandler.Workflow.HandleUpdateWorkflow)
	mux.HandleFunc("DELETE /v1/workflows/{id}", s.sysadminHandler.Workflow.HandleDeleteWorkflow)
	mux.HandleFunc("GET /v1/workflows/{id}/runs", s.sysadminHandler.Workflow.HandleListWorkflowRuns)
	mux.HandleFunc("POST /v1/workflows/{id}/trigger", s.sysadminHandler.Workflow.HandleTriggerWorkflow)

	// 聊天平台集成 API
	mux.HandleFunc("GET /v1/channels", s.sysadminHandler.Channels.HandleListChannels)
	mux.HandleFunc("POST /v1/channels", s.sysadminHandler.Channels.HandleCreateChannel)
	mux.HandleFunc("PUT /v1/channels/{channelID}", s.sysadminHandler.Channels.HandleUpdateChannel)
	mux.HandleFunc("DELETE /v1/channels/{channelID}", s.sysadminHandler.Channels.HandleDeleteChannel)
	// 2026-07-07 修复：webhook 接收路由此前从未注册（HandleWebhookReceive 是
	// 完整实现但从未接线的死代码），导致 Slack/Discord/Telegram/LINE/WhatsApp/
	// Teams/通用 HMAC 全部 webhook 集成在生产环境完全不可达。GET 用于 WhatsApp
	// hub.challenge 握手（verifyWhatsAppWebhook 内部按 r.Method 分支），其余
	// 平台走 POST。
	mux.HandleFunc("POST /v1/webhooks/{channelType}/{channelID}", s.sysadminHandler.Channels.HandleWebhookReceive)
	mux.HandleFunc("GET /v1/webhooks/{channelType}/{channelID}", s.sysadminHandler.Channels.HandleWebhookReceive)

	// App Sandbox 生命周期 API (M13)
	mux.HandleFunc("GET /v1/apps", s.sysadminHandler.HandleListApps)
	mux.HandleFunc("POST /v1/apps", s.sysadminHandler.HandleCreateApp)
	mux.HandleFunc("GET /v1/apps/{id}", s.sysadminHandler.HandleGetApp)
	mux.HandleFunc("PUT /v1/apps/{id}", s.sysadminHandler.HandleUpdateApp)
	mux.HandleFunc("DELETE /v1/apps/{id}", s.sysadminHandler.HandleDeleteApp)
	mux.HandleFunc("POST /v1/apps/{id}/enable", s.sysadminHandler.HandleSetAppEnabled)

	// 工具 & Skill 管理 API
	mux.HandleFunc("GET /v1/tools", s.sysadminHandler.HandleListTools)
	// // mux.HandleFunc("...", s.handleListToolSchemas)
	mux.HandleFunc("POST /v1/tools/{name}/execute", s.sysadminHandler.HandleExecuteTool)
	mux.HandleFunc("GET /v1/skills", s.sysadminHandler.HandleListSkills)
	mux.HandleFunc("POST /v1/skills/install", s.sysadminHandler.HandleInstallSkill)
	// 用户意图驱动的技能生成入口（P3-2 SkillCreator，2026-07-21 deadcode 审查补齐）
	mux.HandleFunc("POST /v1/skills/create", s.sysadminHandler.HandleCreateSkill)

	// MCP Server 管理 API
	mux.HandleFunc("GET /v1/mcp-servers", s.sysadminHandler.MCP.HandleListMCPServers)
	// 2026-07-07 修复：POST（创建）/DELETE（删除）此前只留了指向不存在方法名的
	// 注释占位（HandleAddMCPServer/HandleRemoveMCPServer 从未存在，真正的
	// handler 名为 HandleCreateMCPServer/HandleDeleteMCPServer），导致这两个
	// 完整实现的 handler 从未被注册为路由——无法通过独立 REST API 新增/删除
	// MCP Server（只能走插件安装流程间接创建，且完全无法删除非插件 MCP）。
	mux.HandleFunc("POST /v1/mcp-servers", s.sysadminHandler.MCP.HandleCreateMCPServer)
	mux.HandleFunc("PUT /v1/mcp-servers/{serverID}", s.sysadminHandler.MCP.HandleUpdateMCPServer)
	mux.HandleFunc("DELETE /v1/mcp-servers/{serverID}", s.sysadminHandler.MCP.HandleDeleteMCPServer)

	// [W-6-E] AgentProfile 接入
	mux.HandleFunc("GET /v1/admin/profiles", s.sysadminHandler.HandleListAgentProfiles)
	// [W-6-A] csv_fanout 接入
	mux.HandleFunc("POST /v1/admin/tasks/csv-fanout", s.sysadminHandler.HandleCSVFanout)
	mux.HandleFunc("POST /v1/admin/tasks/pipeline", s.sysadminHandler.HandlePipelineRun)
	mux.HandleFunc("POST /v1/admin/tasks/pattern-dag", s.sysadminHandler.HandlePatternDAGRun)
	mux.HandleFunc("POST /v1/admin/tasks/pattern-mapreduce", s.sysadminHandler.HandleMapReduceRun)
	mux.HandleFunc("POST /v1/admin/tasks/pattern-parallel", s.sysadminHandler.HandleParallelRun)
	mux.HandleFunc("POST /v1/admin/tasks/pattern-sequential", s.sysadminHandler.HandleSequentialRun)
	mux.HandleFunc("POST /v1/admin/tasks/pattern-swarm", s.sysadminHandler.HandleSwarmRun)
	mux.HandleFunc("POST /v1/mcp-servers/{serverID}/test", s.sysadminHandler.MCP.HandleTestMCPServer)
	// 网络访问审批：PUT /v1/mcp-servers/{id}/network-access  body: {"approved": true/false}
	mux.HandleFunc("PUT /v1/mcp-servers/{serverID}/network-access", s.sysadminHandler.MCP.HandleMCPNetworkApproval)

	// 插件目录 API
	mux.HandleFunc("GET /v1/plugins/catalog", s.pluginHandler.HandleListPluginCatalog)
	mux.HandleFunc("POST /v1/plugins/install", s.pluginHandler.HandleInstallPlugin)
	mux.HandleFunc("DELETE /v1/plugins/{catalogID}", s.pluginHandler.HandleUninstallPlugin)

	// 已安装插件管理 API（对接 plugins 运行时表）
	mux.HandleFunc("GET /v1/plugins", s.pluginHandler.HandleListPlugins)
	mux.HandleFunc("PUT /v1/plugins/{id}", s.pluginHandler.HandleUpdatePlugin)
	mux.HandleFunc("POST /v1/plugins/{id}/toggle", s.pluginHandler.HandleTogglePluginMCP)
	mux.HandleFunc("POST /v1/plugins/{id}/upgrade", s.pluginHandler.HandleUpgradePlugin)

	// Custom Entity Creation
	// mux.HandleFunc("...", s.handleCreateMCP)
	// mux.HandleFunc("...", s.sysadminHandler.HandleCreateSkill)
	// mux.HandleFunc("...", s.handleCreatePlugin)
	// mux.HandleFunc("...", s.handleCreateApp)

	// 插件市场 API
	mux.HandleFunc("GET /v1/plugins/marketplaces", s.pluginHandler.HandleListMarketplaces)
	mux.HandleFunc("POST /v1/plugins/marketplaces", s.pluginHandler.HandleAddMarketplace)
	mux.HandleFunc("DELETE /v1/plugins/marketplaces/{id}", s.pluginHandler.HandleDeleteMarketplace)
	mux.HandleFunc("POST /v1/plugins/marketplaces/sync", s.pluginHandler.HandleSyncMarketplaces)
	// /v1/plugins/sync 是 /v1/plugins/marketplaces/sync 的前端别名（Web UI plugins.js 硬编码路径）
	mux.HandleFunc("POST /v1/plugins/sync", s.pluginHandler.HandleSyncMarketplaces)

	// OpenAI 兼容端点（允许第三方 OpenAI SDK 客户端直接对接）
	// mux.HandleFunc("...", s.handleOpenAIChat)

	// 预算管理
	mux.HandleFunc("GET /v1/config/budget", s.sysadminHandler.HandleGetBudget)
	mux.HandleFunc("PUT /v1/config/budget", s.sysadminHandler.HandleSetBudget)

	// 系统备份 / 恢复
	mux.HandleFunc("GET /v1/export/backup", s.sysadminHandler.HandleExportBackup)
	mux.HandleFunc("POST /v1/import/backup", s.sysadminHandler.HandleImportBackup)

	// 系统版本 & OTA 热更新（前端直接调 GitHub API 检查版本，后端只负责执行更新）
	mux.HandleFunc("GET /v1/system/version", s.sysadminHandler.HandleGetVersion)
	mux.HandleFunc("POST /v1/system/update", s.sysadminHandler.HandleTriggerUpdate)

}
