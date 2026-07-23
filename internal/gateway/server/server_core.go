package server

import (
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	prepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/tool/catalog"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/gateway/server/plugin"
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/channelsadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"

	"context"
	"sync/atomic"

	"net/http"

	"golang.org/x/time/rate"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/internal/sysmgr/updater"
	"github.com/polarisagi/polaris/pkg/types"
)

// Server 包装 HTTP 与 WebSocket 服务，作为 M13 的对外网关。
type Server struct {
	addr           string
	srv            *http.Server
	isReady        atomic.Bool
	agentPool      protocol.AgentPool
	blackboard     protocol.Blackboard
	pipelineOrch   *orchestrator.PipelineOrchestrator
	patternDAGExec *orchestrator.PatternDAGExecutor
	mapReduceExec  *orchestrator.MapReduceExecutor
	parallelExec   *orchestrator.ParallelExecutor
	sequentialExec *orchestrator.SequentialExecutor
	swarmCoord     *orchestrator.SwarmCoordinator
	hitlGateway    protocol.HITL
	db             protocol.SQLQuerier
	chatRepo       protocol.ChatRepository
	providerRepo   protocol.ProviderRepository
	extRepo        protocol.ExtensionRepository
	budgetRepo     prepo.BudgetRepository
	systemRepo     prepo.SystemRepository
	channelRepo    prepo.ChannelRepository
	automationRepo prepo.AutomationRepository
	eventRepo      prepo.EventRepository
	cronRepo       protocol.CronRepository
	workflowRepo   prepo.WorkflowRepository
	appRepo        prepo.AppRepository
	registry       protocol.LLMRegistry  // 热重载 Provider 注册表（接口，禁止直接持有 *llm.ProviderRegistry）
	httpClient     *http.Client          // 复用 SafeHTTPClient
	transcriptDir  string                // per-session JSONL transcript 目录
	hooks          *sysadmin.HookRunner  // Shell Script Hooks（End-User 扩展点）
	compressor     *chat.Compressor      // 上下文超长自动压缩
	channelMgr     ChannelStarter        // 所有聊天平台 poller 管理（接口）
	mcpMgr         MCPManager            // MCP Server 连接管理（接口）
	toolReg        protocol.ToolRegistry // builtin tool 元数据
	catalog        catalog.Catalog
	skillReg       protocol.SkillRegistry                                                         // skill 元数据
	toolExec       func(ctx context.Context, name string, args []byte) (*types.ToolResult, error) // tool_use 执行器
	logStore       *LogStore                                                                      // 日志环形缓冲 + SSE 广播
	evalRunner     protocol.EvalRunner                                                            // M12 评测套件
	dataDir        string                                                                         // 项目统一的数据根目录
	installMgr     ExtensionInstaller                                                             // 扩展安装/卸载管理器（接口）
	pluginCreator  plugin.PluginGenerator                                                         // LLM 驱动 MCP 插件自动生成（M2 PluginCreator，消费端接口）
	scriptRunner   marketplace.HookRunner                                                         // install hook 沙箱执行器（ContainerSandbox.RunScript）
	skillSignKey   []byte

	updater *updater.Manager     // OTA 自更新管理器（可为 nil）
	ks      *security.KillSwitch // [B1] KillSwitch

	// 系统提示词组装缓存（启动时一次性加载，运行期不变）
	soulMDContent       string                // ~/.polarisagi/polaris/config/SOUL.md 内容
	serverPlatform      string                // 接入平台标识，决定平台感知提示词（cli/webui/api/cron）
	promptMgr           protocol.PromptFacade // 提示词管理器（接口）
	baseSystemPromptTpl string                // sysTmpl 基础值，每轮请求重置 ic.SystemPromptTemplate 防止 ambient 累积

	// M9 激活的系统提示词（从 DB prompt_versions 表读取，Activate 回调热更新）
	activatedSystemPrompt string // task_type='general' 的激活版本

	// Cron runner 生命周期控制
	cronCancel context.CancelFunc
	// workflowStepWorkerCancel workflow_step 自订阅 Worker 生命周期控制（2026-07-12
	// StateGraphExecutor workflow 接入，见 server_lifecycle.go Start/Shutdown）。
	workflowStepWorkerCancel context.CancelFunc

	tbr *metrics.TokenBurnRate

	// lastEventOffset 记录上次 eventTick 已处理的最大 events.offset，防止重复触发。

	rateLimiter      *rate.Limiter
	interruptLimiter *RateLimitManager
	auditTrail       AuditRecorder
	outboxWriter     protocol.OutboxWriter // Interrupt 异步路由（nil 时降级为进程内直调）
	providerHandler  *provider.ProviderHandler
	pluginHandler    *plugin.PluginHandler
	chatHandler      *chat.ChatHandler
	sysadminHandler  *sysadmin.SysAdminHandler
	codeActEngine    CodeActEngine // LLM 生成代码执行引擎门面（可为 nil，降级拒绝）
}

func (s *Server) SetAuditTrail(at AuditRecorder) { s.auditTrail = at }

// ChannelsAdmin 返回底层管理的 ChannelsAdmin，供 boot 阶段获取并给 channelMgr 绑定 handler
func (s *Server) ChannelsAdmin() *channelsadmin.ChannelsAdmin {
	if s.sysadminHandler != nil {
		return s.sysadminHandler.Channels
	}
	return nil
}

// SetOutboxWriter 注入 Outbox 写入器。此前该 setter 在 cmd/polaris 启动流程中
// 从未被调用过——s.outboxWriter 恒为 nil，导致 handleAgentInterrupt 的
// Interrupt 异步路由（server_handlers_hitl.go）永远退化为"outboxWriter 未注入，
// 无法直调"分支（2026-07-12 复核发现，boot_server.go 现已补上调用）。
// 同时转发给 chatHandler.OutboxWriter，供 SaveMessage 的持久化重试兜底使用
// （GD-13-004 复核修复）——二者共用同一个 Outbox 实例，无需重复注入。
func (s *Server) SetOutboxWriter(w protocol.OutboxWriter) {
	s.outboxWriter = w
	if s.chatHandler != nil {
		s.chatHandler.OutboxWriter = w
	}
}

func (s *Server) SetPipelineOrchestrator(po *orchestrator.PipelineOrchestrator) {
	s.pipelineOrch = po
	if s.sysadminHandler != nil {
		s.sysadminHandler.PipelineOrch = po
	}
}

func (s *Server) SetPatternDAGExecutor(pe *orchestrator.PatternDAGExecutor) {
	s.patternDAGExec = pe
	if s.sysadminHandler != nil {
		s.sysadminHandler.PatternDAGExec = pe
	}
}

func (s *Server) SetMapReduceExecutor(me *orchestrator.MapReduceExecutor) {
	s.mapReduceExec = me
	if s.sysadminHandler != nil {
		s.sysadminHandler.MapReduceExec = me
	}
}

func (s *Server) SetParallelExecutor(pe *orchestrator.ParallelExecutor) {
	s.parallelExec = pe
	if s.sysadminHandler != nil {
		s.sysadminHandler.ParallelExec = pe
	}
}

func (s *Server) SetSequentialExecutor(se *orchestrator.SequentialExecutor) {
	s.sequentialExec = se
	if s.sysadminHandler != nil {
		s.sysadminHandler.SequentialExec = se
	}
}

func (s *Server) SetSwarmCoordinator(sc *orchestrator.SwarmCoordinator) {
	s.swarmCoord = sc
	if s.sysadminHandler != nil {
		s.sysadminHandler.SwarmCoord = sc
	}
}

func (s *Server) SetInstallManager(m ExtensionInstaller) {
	s.installMgr = m
	if s.sysadminHandler != nil {
		s.sysadminHandler.InstallMgr = m
		// 2026-07-07 mcp_servers.go 拆分为 mcpadmin 子包后需要单独回填，
		// 否则 MCP Server 创建/更新走 PolicyGate 授权时永远 fail-closed 拒绝
		// （见 mcpadmin/mcp_servers.go 的 h.InstallMgr == nil 检查）。
		s.sysadminHandler.MCP.InstallMgr = m
	}
	if s.pluginHandler != nil {
		s.pluginHandler.InstallMgr = m
	}
}

func (s *Server) SetPluginCreator(pc plugin.PluginGenerator) {
	s.pluginCreator = pc
	if s.pluginHandler != nil {
		s.pluginHandler.PluginCreator = pc
	}
}

// SetPersonaRefiner 注入用户画像精炼器（M05 §2.3），与 internal/agent 共享同一
// 进程级单例（cmd/polaris/boot_agent.go 构造 + Load）。nil 时 ChatHandler
// 跳过用户偏好画像注入（消费端见 chat/system_prompt.go）。
func (s *Server) SetPersonaRefiner(pr *agentctx.PersonaRefiner) {
	if s.chatHandler != nil {
		s.chatHandler.PersonaRefiner = pr
	}
}

// SetSamplingMonitor 见 server_setters_sampling.go（R7 拆分：server_core.go
// 加入该 setter 会突破 400 行上限）。

// SetEmbedder 注入语义向量化引擎（Tier 2 Ambient 匹配 + tool schema 语义过滤）。
// nil 时 ChatHandler/SysAdminHandler 均自动降级 Tier 1（全量 schema 注入）。
func (s *Server) SetEmbedder(e search.Embedder, threshold float64) {
	if s.chatHandler != nil {
		s.chatHandler.Embedder = e
		if threshold > 0 {
			s.chatHandler.EmbedThreshold = threshold
		} else {
			s.chatHandler.EmbedThreshold = 0.60 // 默认阈值
		}
	}
	// SysAdminHandler 使用同一 Embedder 做 tool schema 语义过滤（>40 工具时激活）
	if s.sysadminHandler != nil {
		s.sysadminHandler.Embedder = e
	}
}

func (s *Server) SetEmbeddingIndexer(idx *plugin.EmbeddingIndexer) {
	if s.pluginHandler != nil {
		s.pluginHandler.EmbeddingIndexer = idx
	}
}

// SetKillSwitch removed SetEvalRunner to fix duplicate and undefined method error

func (s *Server) SetKillSwitch(ks *security.KillSwitch) {
	s.ks = ks
	if s.sysadminHandler != nil {
		s.sysadminHandler.KillSwitch = ks
	}
}

func (s *Server) SetScriptRunner(r marketplace.HookRunner) {
	s.scriptRunner = r
	if s.pluginHandler != nil {
		s.pluginHandler.ScriptRunner = r
	}
}

func (s *Server) SetSkillSigningKey(k []byte) {
	s.skillSignKey = k
	if s.sysadminHandler != nil {
		s.sysadminHandler.SkillSignKey = k
	}
}

func (s *Server) SetUpdater(u *updater.Manager) {
	s.updater = u
	if s.sysadminHandler != nil {
		s.sysadminHandler.Updater = u
	}
}

// SetCodeActEngine 注入 CodeAct 执行引擎门面（CodeActEngine）。在 NewServer 之后、Serve 之前调用。
// af 为 nil 时 POST /v1/agent/codeact 返回 503。
func (s *Server) SetCodeActEngine(af CodeActEngine) { s.codeActEngine = af }

// SetMCPManager 注入 MCPManager（NewServer 之后、Start 之前调用）。
// 同时注册缓存失效回调：异步插件 MCP 连接完成时自动清除 toolSchemaCache，
// 确保 LLM 在下次推理时能看到新工具，而非返回连接前构建的过期缓存。
func (s *Server) SetMCPManager(m MCPManager) {
	s.mcpMgr = m
	m.SetOnToolsChanged(func() {
		if s.sysadminHandler != nil {
			s.sysadminHandler.ClearToolSchemaCache()
		}
	})
	if s.sysadminHandler != nil {
		s.sysadminHandler.MCPMgr = m
	}
	if s.pluginHandler != nil {
		s.pluginHandler.MCPMgr = m
	}
	if s.chatHandler != nil {
		s.chatHandler.MCPMgr = m
	}
}

// SetToolRegistry 注入 ToolRegistry（NewServer 之后、Start 之前调用）。
func (s *Server) SetToolRegistry(r protocol.ToolRegistry) {
	s.toolReg = r
	if s.chatHandler != nil {
		s.chatHandler.ToolReg = r
	}
}

// SetReloadProviders 注入重新加载提供商的函数
func (s *Server) SetReloadProviders(f func()) {
	if s.providerHandler != nil {
		s.providerHandler.ReloadProviders = f
	}
}

// SetCatalog 注入工具目录
func (s *Server) SetCatalog(c catalog.Catalog) {
	s.catalog = c
	if s.sysadminHandler != nil {
		s.sysadminHandler.Catalog = c
	}
}

// SetSyncSkillFunc 注入运行时同步技能到 ToolRegistry 的回调函数。
func (s *Server) SetSyncSkillFunc(f func(slug, instructions string)) {
	if s.pluginHandler != nil {
		s.pluginHandler.SyncSkillToToolRegistry = f
	}
}

// SetSkillRegistry 注入 SkillRegistry（NewServer 之后、Start 之前调用）。
func (s *Server) SetSkillRegistry(r protocol.SkillRegistry) {
	s.skillReg = r
	if s.sysadminHandler != nil {
		s.sysadminHandler.SkillReg = r
	}
	if s.pluginHandler != nil {
		s.pluginHandler.SkillReg = r
	}
	if s.chatHandler != nil {
		s.chatHandler.SkillReg = r
	}
}

// SetToolExecutor 注入工具执行函数，用于 tool_use 循环（NewServer 之后、Start 之前调用）。
func (s *Server) SetToolExecutor(fn func(ctx context.Context, name string, args []byte) (*types.ToolResult, error)) {
	s.toolExec = fn
	if s.sysadminHandler != nil {
		s.sysadminHandler.ToolExec = fn
		s.sysadminHandler.Cron.ToolExec = fn
		// 2026-07-07 workflow.go/channels.go 拆分为独立子包后需要单独回填，
		// 否则 workflow 步骤 / 频道消息分发里的 tool_use 循环永远拿不到
		// ToolExec（见 workflowadmin/workflow_engine.go、
		// channelsadmin/webhook_receive.go 的 h.ToolExec nil check）。
		s.sysadminHandler.Workflow.ToolExec = fn
		s.sysadminHandler.Channels.ToolExec = fn
	}
}

// SetLogStore 注入日志存储（NewServer 之后、Start 之前调用）。
func (s *Server) SetLogStore(ls *LogStore) { s.logStore = ls }

// SetEvalRunner 注入 M12 评测套件（NewServer 之后、Start 之前调用）。
func (s *Server) SetEvalRunner(r protocol.EvalRunner) { s.evalRunner = r }

// SetEvalAdmin 见 server_setters_eval.go（R7 拆分：server_core.go 逼近 400 行上限）。

// SetToolRefOffloader 注入 ToolRefOffloader 到 Compressor（NewServer 之后、Start 之前调用）。
func (s *Server) SetToolRefOffloader(offloader chat.ToolRefOffloader) {
	if s.compressor != nil {
		s.compressor.SetToolRefOffloader(offloader)
	}
}

// NewServer / Start / Shutdown 见 server_lifecycle.go（R7 拆分）。

func (s *Server) PluginHandler() *plugin.PluginHandler {
	return s.pluginHandler
}

func (s *Server) SetPromptManager(mgr protocol.PromptFacade) {
	s.promptMgr = mgr
	s.soulMDContent = s.promptMgr.GetSoulMD()

	// 初始化 system_prompt_template
	prefs, _ := s.systemRepo.ListPreferences(context.Background())
	sysTmpl, ok := prefs["system_prompt_template"]
	// 如果之前因为 bug 导致数据库缓存了残缺兜底版，强制覆盖
	if !ok || sysTmpl == "你是 {{.AgentName}}，{{.AgentRole}}。\n当前运行模型：{{.ModelID}}。" {
		sysTmpl = s.promptMgr.ReadPromptDefault("system_prompt.md")
		if sysTmpl == protocol.DefaultPolarisIdentityFallback {
			sysTmpl = "你是 {{.AgentName}}，{{.AgentRole}}。\n当前运行模型：{{.ModelID}}。"
		}
		_ = s.systemRepo.UpsertPreference(context.Background(), "system_prompt_template", sysTmpl)
	}
	s.baseSystemPromptTpl = sysTmpl

	if s.chatHandler != nil {
		s.chatHandler.PromptMgr = mgr
		s.chatHandler.BaseSystemPromptTpl = s.baseSystemPromptTpl
	}
}

func (s *Server) SetChannelStarter(mgr ChannelStarter) {
	s.channelMgr = mgr
}

func (s *Server) SetInferenceRouter(router protocol.Provider) {
	if s.sysadminHandler != nil {
		s.sysadminHandler.Router = router
	}
}

func (s *Server) SetSTTProvider(provider chat.STTTranscriber) {
	if s.chatHandler != nil {
		s.chatHandler.SetSTTEngine(provider)
	}
}

func (s *Server) SetTTSProvider(provider chat.TTSProvider) {
	if s.chatHandler != nil {
		s.chatHandler.SetTTSEngine(provider)
	}
}

// SetWorktreeManagerFactory 注入工作区管理器工厂
func (s *Server) SetWorktreeManagerFactory(f func(workingDir, worktreeRoot string) sysadmin.WorktreeManager) {
	if s.sysadminHandler != nil {
		s.sysadminHandler.NewWorktreeManager = f
		s.sysadminHandler.Cron.NewWorktreeManager = func(workingDir, worktreeRoot string) cronadmin.WorktreeManager {
			return f(workingDir, worktreeRoot)
		}
	}
}
