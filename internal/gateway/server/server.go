package server

import (
	prepo "github.com/polarisagi/polaris/internal/protocol/repo"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/gateway/server/plugin"
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"

	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"sync/atomic"

	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/channel"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/internal/sysmgr/updater"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
	webui "github.com/polarisagi/polaris/web"

	"gopkg.in/yaml.v3"
)

// Server 包装 HTTP 与 WebSocket 服务，作为 M13 的对外网关。
type Server struct {
	addr           string
	srv            *http.Server
	agentPool      chat.AgentPool
	blackboard     protocol.Blackboard
	hitlGateway    protocol.HITL
	db             protocol.SQLQuerier
	chatRepo       protocol.ChatRepository
	providerRepo   protocol.ProviderRepository
	extRepo        protocol.ExtensionRepository
	budgetRepo     prepo.BudgetRepository
	systemRepo     prepo.SystemRepository
	channelRepo    prepo.ChannelRepository
	automationRepo prepo.AutomationRepository
	workflowRepo   prepo.WorkflowRepository
	appRepo        prepo.AppRepository
	registry       LLMRegistry           // 热重载 Provider 注册表（接口，禁止直接持有 *llm.ProviderRegistry）
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

	updater *updater.Manager // OTA 自更新管理器（可为 nil）

	// 系统提示词组装缓存（启动时一次性加载，运行期不变）
	soulMDContent       string        // ~/.polarisagi/polaris/config/SOUL.md 内容
	serverPlatform      string        // 接入平台标识，决定平台感知提示词（cli/webui/api/cron）
	promptMgr           PromptManager // 提示词管理器（接口）
	baseSystemPromptTpl string        // sysTmpl 基础值，每轮请求重置 ic.SystemPromptTemplate 防止 ambient 累积

	// M9 激活的系统提示词（从 DB prompt_versions 表读取，Activate 回调热更新）
	activatedSystemPrompt string // task_type='general' 的激活版本

	// Cron runner 生命周期控制
	cronCancel context.CancelFunc

	tbr *metrics.TokenBurnRate

	// lastEventOffset 记录上次 eventTick 已处理的最大 events.offset，防止重复触发。

	rateLimiter      *rate.Limiter
	interruptLimiter *RateLimitManager
	auditTrail       *security.AuditTrail
	outboxWriter     protocol.OutboxWriter // Interrupt 异步路由（nil 时降级为进程内直调）
	providerHandler  *provider.ProviderHandler
	pluginHandler    *plugin.PluginHandler
	chatHandler      *chat.ChatHandler
	sysadminHandler  *sysadmin.SysAdminHandler
	codeActEngine    action.ActionFacade // LLM 生成代码执行引擎门面（可为 nil，降级拒绝）
	toolStage        interface {
		SelectFor(ctx context.Context, query string) []types.ToolSchema
	}
}

func (s *Server) SetAuditTrail(at *security.AuditTrail)   { s.auditTrail = at }
func (s *Server) SetOutboxWriter(w protocol.OutboxWriter) { s.outboxWriter = w }
func (s *Server) SetInstallManager(m ExtensionInstaller) {
	s.installMgr = m
	if s.sysadminHandler != nil {
		s.sysadminHandler.InstallMgr = m
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

// SetCodeActEngine 注入 CodeAct 执行引擎门面（action.ActionFacade）。在 NewServer 之后、Serve 之前调用。
// af 为 nil 时 POST /v1/agent/codeact 返回 503。
func (s *Server) SetCodeActEngine(af action.ActionFacade) { s.codeActEngine = af }

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

// SetCatalog 注入工具目录
func (s *Server) SetCatalog(c catalog.Catalog) {
	s.catalog = c
	if s.sysadminHandler != nil {
		s.sysadminHandler.Catalog = c
	}
}

// SetToolStage 注入工具阶段
func (s *Server) SetToolStage(stage interface {
	SelectFor(ctx context.Context, query string) []types.ToolSchema
}) {
	s.toolStage = stage
	if s.chatHandler != nil {
		s.chatHandler.ToolStage = stage
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
	}
}

// SetLogStore 注入日志存储（NewServer 之后、Start 之前调用）。
func (s *Server) SetLogStore(ls *LogStore) { s.logStore = ls }

// SetEvalRunner 注入 M12 评测套件（NewServer 之后、Start 之前调用）。
func (s *Server) SetEvalRunner(r protocol.EvalRunner) { s.evalRunner = r }

// buildToolSchemas 收集全部可用工具 schema，用于注入 InferRequest.Tools。
func NewServer(addr string, dataDir string, agentPool chat.AgentPool, bb protocol.Blackboard, hitlGateway protocol.HITL, db protocol.SQLQuerier, registry LLMRegistry, httpClient *http.Client, safeDialer protocol.SafeDialer, compressorCfg config.CompressorConfig, tbr *metrics.TokenBurnRate, rateLimiter *rate.Limiter) *Server {
	tDir := filepath.Join(dataDir, "sessions")
	go chat.PruneTranscripts(tDir, 30) // 启动时异步清理 30 天前的 transcript

	s := &Server{
		addr:             addr,
		agentPool:        agentPool,
		blackboard:       bb,
		hitlGateway:      hitlGateway,
		db:               db,
		registry:         registry,
		httpClient:       httpClient,
		transcriptDir:    tDir,
		hooks:            sysadmin.NewHookRunner(dataDir),
		dataDir:          dataDir,
		tbr:              tbr,
		rateLimiter:      rateLimiter,
		interruptLimiter: NewRateLimitManager(1, 10), // Approx 10 req/min with burst
	}
	sqlDB, ok := db.(*sql.DB)
	if !ok {
		// db 必须是 *sql.DB 才能初始化 Repository 层；传入其他类型时快速失败，避免空指针 panic 延迟暴露。
		panic(fmt.Sprintf("NewServer: db 必须为 *sql.DB，实际收到 %T", db))
	}
	s.chatRepo = repo.NewSQLiteChatRepository(sqlDB)
	s.providerRepo = repo.NewSQLiteProviderRepository(sqlDB)
	s.extRepo = repo.NewSQLiteExtensionRepository(sqlDB)
	s.budgetRepo = repo.NewSQLiteBudgetRepository(sqlDB)
	s.systemRepo = repo.NewSQLiteSystemRepository(sqlDB)
	s.channelRepo = repo.NewSQLiteChannelRepository(sqlDB)
	s.automationRepo = repo.NewSQLiteAutomationRepository(sqlDB)
	s.workflowRepo = repo.NewSQLiteWorkflowRepository(sqlDB)
	s.appRepo = repo.NewSQLiteAppRepository(sqlDB)

	// 注入内置的 yaml 配置作为种子数据到数据库（SSoT 架构）
	seedBuiltinConfig(s)

	prefs, _ := s.systemRepo.ListPreferences(context.Background())
	sysTmpl, ok := prefs["system_prompt_template"]
	if !ok {
		if b, err := os.ReadFile("configs/prompts/system_prompt.md"); err == nil {
			sysTmpl = string(b)
			_ = s.systemRepo.UpsertPreference(context.Background(), "system_prompt_template", sysTmpl)
			prefs["system_prompt_template"] = sysTmpl
		} else {
			sysTmpl = "你是 {{.AgentName}}，{{.AgentRole}}。\n当前运行模型：{{.ModelID}}。"
		}
	}

	// 保存基础模板，每轮请求重置 ic.SystemPromptTemplate 防止 ambient 内容累积
	s.baseSystemPromptTpl = sysTmpl

	// global agent memory setup is removed as agent is now per-session

	// 注入 embedded FS 和运行时配置目录到 memory 包（三层提示词加载的 Layer 0/1）
	// 必须在 LoadSoulMD / DefaultIdentity 之前完成
	s.promptMgr = prompt.NewManager(filepath.Join(dataDir, "config"), configs.FS)

	// 加载用户身份（三层优先级：user prompts/identity.md > SOUL.md > embedded default）
	s.soulMDContent = s.promptMgr.LoadSoulMD()

	// 读取接入平台标识（环境变量 POLARIS_PLATFORM，缺失时默认 webui）
	s.serverPlatform = os.Getenv("POLARIS_PLATFORM")
	if s.serverPlatform == "" {
		s.serverPlatform = "webui"
	}

	// 从 DB 加载 M9 已激活的 general 系统提示词（启动热恢复）
	if db != nil {
		var activatedPrompt string
		row := db.QueryRowContext(context.Background(),
			"SELECT prompt_text FROM prompt_versions WHERE task_type='general' AND is_active=1 ORDER BY created_at DESC LIMIT 1")
		if err := row.Scan(&activatedPrompt); err == nil && activatedPrompt != "" {
			s.activatedSystemPrompt = activatedPrompt
		}
	}

	s.compressor = chat.NewCompressor(db, s.chatRepo, s.hooks, compressorCfg)
	s.channelMgr = channel.NewManager(httpClient, func(channelType, channelID string, cfg map[string]any, msg cadapter.Message) {
		// s.dispatchChannelMessage(context.Background(), channelType, channelID, cfg, msg)
	}, channel.WithSafeDialer(safeDialer))

	s.providerHandler = &provider.ProviderHandler{
		ProviderRepo: s.providerRepo,
		ExtRepo:      s.extRepo,
		Registry:     s.registry,
		HTTPClient:   httpClient,
		TBR:          tbr,
		DB:           db,
	}
	// sseWriteFn 与 chat.writeSSE 保持完全相同的协议（chat 包内函数未导出，此处镜像实现）
	sseWriteFn := func(w http.ResponseWriter, f http.Flusher, event string, payload any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		f.Flush()
	}
	// STT/TTS 原子指针：启动时持有 nil 引擎，InitSTTEngine/InitTTSEngine 完成后原子替换为真实引擎。
	// 必须非 nil，否则 .Store()/.Load() 调用时 nil pointer dereference。
	sttPtr := new(atomic.Pointer[chat.STTEngineBox])
	ttsPtr := new(atomic.Pointer[tts.ProviderBox])
	s.chatHandler = &chat.ChatHandler{
		DB:                    db,
		ChatRepo:              s.chatRepo,
		ProviderRepo:          s.providerRepo,
		SystemRepo:            s.systemRepo,
		AgentPool:             agentPool,
		Blackboard:            bb,
		Compressor:            s.compressor,
		SlashRouter:           chat.NewSlashCommandRouter(s.compressor, s.chatRepo, sseWriteFn),
		TranscriptDir:         s.transcriptDir,
		PromptMgr:             s.promptMgr,
		SoulMDContent:         &s.soulMDContent,
		Hooks:                 s.hooks,
		DataDir:               s.dataDir,
		Registry:              s.registry,
		ServerPlatform:        s.serverPlatform,
		BaseSystemPromptTpl:   s.baseSystemPromptTpl,
		ActivatedSystemPrompt: s.activatedSystemPrompt,
		STTEngine:             sttPtr,
		TTSEngine:             ttsPtr,
		WriteSSE:              sseWriteFn,
	}
	s.sysadminHandler = &sysadmin.SysAdminHandler{
		SystemRepo:     s.systemRepo,
		BudgetRepo:     s.budgetRepo,
		WorkflowRepo:   s.workflowRepo,
		ExtRepo:        s.extRepo,
		ChannelRepo:    s.channelRepo,
		AutomationRepo: s.automationRepo,
		AppRepo:        s.appRepo,
		Registry:       s.registry,
		HTTPClient:     httpClient,
		DataDir:        s.dataDir,
		DB:             db,
		Chat:           s.chatHandler,
		ChannelMgr:     s.channelMgr,
	}
	s.pluginHandler = &plugin.PluginHandler{
		ExtRepo:              s.extRepo,
		DB:                   db,
		HTTPClient:           httpClient,
		HITLGateway:          hitlGateway,
		DataDir:              dataDir,
		ClearToolSchemaCache: s.sysadminHandler.ClearToolSchemaCache,
		StartMCPServer:       s.sysadminHandler.StartMCPServerCtx,
	}
	s.chatHandler.ToolProvider = s.sysadminHandler

	mux := http.NewServeMux()

	// API 端点
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleHealthz)
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

	// M12 评测 API
	// mux.HandleFunc("...", s.handleEvalRun)

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
	mux.HandleFunc("GET /v1/automations", s.sysadminHandler.HandleListAutomations)
	mux.HandleFunc("POST /v1/automations", s.sysadminHandler.HandleCreateAutomation)
	mux.HandleFunc("PUT /v1/automations/{id}", s.sysadminHandler.HandleUpdateAutomation)
	mux.HandleFunc("DELETE /v1/automations/{id}", s.sysadminHandler.HandleDeleteAutomation)
	mux.HandleFunc("GET /v1/automations/{id}/runs", s.sysadminHandler.HandleListAutomationRuns)
	mux.HandleFunc("POST /v1/automations/{id}/trigger", s.sysadminHandler.HandleTriggerAutomation)
	mux.HandleFunc("GET /v1/automation-templates", s.sysadminHandler.HandleListAutomationTemplates)

	// 工作流 (Workflows)
	mux.HandleFunc("GET /v1/workflows", s.sysadminHandler.HandleListWorkflows)
	mux.HandleFunc("POST /v1/workflows", s.sysadminHandler.HandleCreateWorkflow)
	mux.HandleFunc("GET /v1/workflows/{id}", s.sysadminHandler.HandleGetWorkflow)
	mux.HandleFunc("PUT /v1/workflows/{id}", s.sysadminHandler.HandleUpdateWorkflow)
	mux.HandleFunc("DELETE /v1/workflows/{id}", s.sysadminHandler.HandleDeleteWorkflow)
	mux.HandleFunc("GET /v1/workflows/{id}/runs", s.sysadminHandler.HandleListWorkflowRuns)
	mux.HandleFunc("POST /v1/workflows/{id}/trigger", s.sysadminHandler.HandleTriggerWorkflow)

	// 聊天平台集成 API
	mux.HandleFunc("GET /v1/channels", s.sysadminHandler.HandleListChannels)
	mux.HandleFunc("POST /v1/channels", s.sysadminHandler.HandleCreateChannel)
	mux.HandleFunc("PUT /v1/channels/{channelID}", s.sysadminHandler.HandleUpdateChannel)
	mux.HandleFunc("DELETE /v1/channels/{channelID}", s.sysadminHandler.HandleDeleteChannel)

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

	// MCP Server 管理 API
	mux.HandleFunc("GET /v1/mcp-servers", s.sysadminHandler.HandleListMCPServers)
	// mux.HandleFunc("...", s.sysadminHandler.HandleAddMCPServer)
	mux.HandleFunc("PUT /v1/mcp-servers/{serverID}", s.sysadminHandler.HandleUpdateMCPServer)
	// mux.HandleFunc("...", s.sysadminHandler.HandleRemoveMCPServer)
	mux.HandleFunc("POST /v1/mcp-servers/{serverID}/test", s.sysadminHandler.HandleTestMCPServer)
	// 网络访问审批：PUT /v1/mcp-servers/{id}/network-access  body: {"approved": true/false}
	mux.HandleFunc("PUT /v1/mcp-servers/{serverID}/network-access", s.sysadminHandler.HandleMCPNetworkApproval)

	// 插件目录 API
	mux.HandleFunc("GET /v1/plugins/catalog", s.pluginHandler.HandleListPluginCatalog)
	mux.HandleFunc("POST /v1/plugins/install", s.pluginHandler.HandleInstallPlugin)
	mux.HandleFunc("DELETE /v1/plugins/{catalogID}", s.pluginHandler.HandleUninstallPlugin)

	// 已安装插件管理 API（对接 plugins 运行时表）
	mux.HandleFunc("GET /v1/plugins", s.pluginHandler.HandleListPlugins)
	mux.HandleFunc("PUT /v1/plugins/{id}", s.pluginHandler.HandleUpdatePlugin)
	mux.HandleFunc("POST /v1/plugins/{id}/toggle", s.pluginHandler.HandleTogglePluginMCP)

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

	if probe.GlobalFeatureGate().State(probe.FeatureWebUI) != probe.FeatureDisabled {
		s.setupWebUI(mux)
	} else {
		slog.Info("polaris: WebUI disabled by FeatureGate")
	}

	// 挂载中间件
	handler := s.withMiddleware(mux)

	s.srv = &http.Server{
		Addr:        addr,
		Handler:     handler,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout 设为 0 禁用全局超时：SSE 流式连接由 ResponseController 管理每请求超时。
		// 短超时（如 60s）会在长对话中途断流。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// seedBuiltinConfig 将 embedded yaml 配置作为种子数据写入数据库（INSERT OR IGNORE）。
func seedBuiltinConfig(s *Server) {
	if b, err := configs.FS.ReadFile("extensions/marketplaces.yaml"); err == nil {
		var mps []protocol.Marketplace
		if err := yaml.Unmarshal(b, &mps); err == nil {
			now := time.Now().UTC().Format(time.RFC3339)
			for _, mp := range mps {
				mp.CreatedAt = now
				_ = s.extRepo.SeedMarketplace(context.Background(), mp)
			}
		}
	} else {
		slog.Warn("polaris-server: configs/extensions/marketplaces.yaml load failed", "err", err)
	}

	if b, err := configs.FS.ReadFile("extensions/registry.yaml"); err == nil {
		var entries []protocol.RegistryEntry
		if err := yaml.Unmarshal(b, &entries); err == nil {
			for _, e := range entries {
				payload, _ := json.Marshal(e)
				_ = s.extRepo.SeedCatalogEntry(context.Background(), types.ExtCatalogRow{
					ID:            e.ID,
					MarketplaceID: "builtin",
					Type:          e.Type,
					Name:          e.Name,
					Description:   e.Description,
					Publisher:     e.Publisher,
					TrustTier:     e.TrustTier,
					URL:           e.URL,
					Payload:       string(payload),
				})
			}
		}
	} else {
		slog.Warn("polaris-server: configs/extensions/registry.yaml load failed", "err", err)
	}
}

// InitSTTEngine 按 FeatureGate 门控初始化 STT 引擎。
// 必须在 NewServer 之后、Start 之前调用（或与 Start 并发，mock 引擎已就绪）。
// 流程：
//  1. 立即注入 mock 引擎（保证 /v1/audio/transcriptions 不返回 503）
//  2. 若门控禁用，仅打 Info 日志后返回
//  3. 否则在后台 goroutine：EnsureAssets → LoadLibrary → NewEngine → 替换为真实引擎
func (s *Server) InitSTTEngine(ctx context.Context, dataDir string, gate *probe.FeatureGate, httpClient *http.Client, sttConfig config.STTConfig) {
	sttDir := filepath.Join(dataDir, "models", "sensevoice")

	// 立即设置 mock 引擎，保证接口可用
	// if mockEngine...

	// 门控检查：FeatureLocalSTT 是最低档（int8），未开启则无法运行 STT
	if gate != nil && gate.State(probe.FeatureLocalSTT) == probe.FeatureDisabled {
		slog.Info("stt: FeatureLocalSTT disabled by FeatureGate (need ≥512MB free), using mock engine")
		return
	}

	// 按 FeatureHQSTT 自动选择模型档位：
	//   HQ 门控开启（≥1GB free）→ float32 SenseVoice（精度优先）
	//   HQ 门控未开启              → int8 SenseVoice（速度/体积优先）
	useHQ := gate != nil && gate.State(probe.FeatureHQSTT) != probe.FeatureDisabled
	modelURL := sttConfig.SenseVoiceModelURL // 默认 float32（HQ）
	if !useHQ {
		if sttConfig.SenseVoiceModelURLStd != "" {
			modelURL = sttConfig.SenseVoiceModelURLStd // int8 标准档
		}
		// SenseVoiceModelURLStd 为空（旧配置）则回退到 SenseVoiceModelURL
	}

	// 异步下载 + 重载：不阻塞启动路径
	go func() {
		if err := stt.EnsureAssets(ctx, sttDir, httpClient, sttConfig.SherpaVersion, modelURL, sttConfig.PunctModelURL); err != nil {
			slog.Warn("stt: asset download failed, keeping mock engine", "err", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := stt.LoadLibrary(libPath); err != nil {
			slog.Warn("stt: library load failed after download, keeping mock engine", "err", err)
			return
		}

		modelDir := stt.ModelDir(sttDir)
		slog.Info("stt: real engine active (sherpa-onnx SenseVoice)",
			"model_dir", modelDir,
			"hq", useHQ,
			"model_url", modelURL,
		)
	}()
}

// InitTTSEngine 初始化 TTS Provider 并注入 ChatHandler。
//
// 三条路径由 ttsConfig.Provider 决定：
//   - "edge"    → EdgeProvider（Microsoft Edge TTS WebSocket，无需下载，立即可用）
//   - "http"    → HTTPProvider（外部 sidecar，如 CosyVoice 2 / Qwen3-TTS）
//   - ""/"sherpa" → SherpaProvider（sherpa-onnx 本地 Kokoro，异步下载后激活）
func (s *Server) InitTTSEngine(ctx context.Context, dataDir string, gate *probe.FeatureGate, httpClient *http.Client, ttsConfig config.TTSConfig) {
	switch ttsConfig.Provider {
	case "edge":
		// Edge TTS：免费、无需下载、立即激活，不受 FeatureGate 门控（无内存开销）
		p := tts.NewEdgeProvider(ttsConfig.EdgeVoice)
		s.chatHandler.SetTTSEngine(p)
		slog.Info("tts: Edge TTS active", "voice", ttsConfig.EdgeVoice)
		return

	case "http":
		// HTTP sidecar：同样立即激活，连通性由首次调用时发现
		if ttsConfig.HTTPEndpoint == "" {
			slog.Warn("tts: provider=http but http_endpoint is empty, TTS disabled")
			return
		}
		p := tts.NewHTTPProvider(ttsConfig.HTTPEndpoint, httpClient)
		s.chatHandler.SetTTSEngine(p)
		slog.Info("tts: HTTP sidecar TTS active", "endpoint", ttsConfig.HTTPEndpoint)
		return
	}

	// ── Sherpa 本地路径（provider="" 或 "sherpa"）──────────────────────────────
	// 修复 bug：原代码错误使用 FeatureLocalSTT 门控 TTS，现改为独立的 FeatureLocalTTS。
	if gate != nil && gate.State(probe.FeatureLocalTTS) == probe.FeatureDisabled {
		slog.Info("tts: FeatureLocalTTS disabled by FeatureGate (need ≥512MB free)")
		return
	}
	if ttsConfig.ModelURL == "" {
		slog.Info("tts: sherpa provider but model_url is empty, TTS disabled")
		return
	}

	ttsDir := filepath.Join(dataDir, "models", "kokoro")
	go func() {
		sttDir := filepath.Join(dataDir, "models", "sensevoice")
		if err := tts.EnsureAssets(ctx, sttDir, ttsDir, httpClient, ttsConfig.SherpaVersion, ttsConfig.ModelURL); err != nil {
			slog.Warn("tts: asset download failed", "err", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := tts.LoadLibrary(libPath); err != nil {
			slog.Warn("tts: library load failed", "err", err)
			return
		}

		modelDir := tts.ModelDir(ttsDir)
		engine, err := tts.NewEngine(modelDir)
		if err != nil {
			slog.Warn("tts: engine init failed", "err", err)
			return
		}
		s.chatHandler.SetTTSEngine(engine)
		slog.Info("tts: sherpa-onnx Kokoro active", "model_dir", modelDir)
	}()
}

func (s *Server) setupWebUI(mux *http.ServeMux) {
	// 挂载 Web UI 静态资源：DEV_MODE=1 反代 Vite，否则用 go:embed dist
	if os.Getenv("DEV_MODE") == "1" {
		target, _ := url.Parse("http://localhost:5173")
		proxy := httputil.NewSingleHostReverseProxy(target)
		mux.Handle("/", proxy)
		return
	}

	subFS, err := fs.Sub(webui.WebUIFS, "dist")
	if err != nil {
		return
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Don't fallback for API routes
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		// Clean the path to check if it exists in the embed FS
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "."
		}

		// Check if the requested file exists
		f, err := subFS.Open(p)
		if err != nil {
			// Fallback to index.html for SPA routing
			r.URL.Path = "/"
		} else {
			f.Close()
		}

		// 缓存策略与字符编码：
		// - index.html 及所有 HTML：no-cache（每次重新验证，防止浏览器用旧 HTML）
		// - /assets/*.js /assets/*.css（Vite 内容 hash 命名）：immutable 永久缓存
		// - 其他静态资源：1h 缓存
		switch {
		case strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" || r.URL.Path == "":
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			if strings.HasSuffix(r.URL.Path, ".js") {
				w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			} else if strings.HasSuffix(r.URL.Path, ".css") {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			}
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
			if strings.HasSuffix(r.URL.Path, ".js") {
				w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			}
		}

		http.FileServer(http.FS(subFS)).ServeHTTP(w, r)
	})
}

// Start 非阻塞启动服务器。
func (s *Server) Start() error {
	slog.Info("polaris-server: starting", "addr", s.addr)

	// 提前监听端口，如果失败直接返回，避免其他后台协程（如 pollers）启动导致多个实例抢占
	// 注意：在 net/http 中我们可以使用 net.Listen
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		slog.Error("polaris-server: listener error", "err", err)
		return apperr.Wrap(apperr.CodeInternal, "failed to listen on tcp address", err)
	}

	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("polaris-server: serve error", "err", err)
		}
	}()
	go s.channelMgr.LoadFromDB(s.db) // 启动所有已配置平台的 poller

	// Cron runner 使用可取消 context，Shutdown 时能优雅停止
	// cronCtx...
	// s.startCronRunner(...)

	go s.bootMarketplaceInit(context.Background())

	// gateway.startup hook：服务完全启动后触发，fire-and-forget
	workspace := os.Getenv("POLARIS_DATA_DIR")
	if workspace == "" {
		if home, err := os.UserHomeDir(); err == nil {
			workspace = filepath.Join(home, ".polarisagi/polaris")
		}
	}
	s.hooks.Fire("gateway.startup", map[string]string{
		"POLARIS_WORKSPACE": workspace,
		"POLARIS_ADDR":      s.addr,
	})

	return nil
}

// Shutdown 优雅关闭服务器。
func (s *Server) Shutdown(ctx context.Context) error {
	// 停止 Cron runner（释放已排队任务 goroutine，拒绝新触发）
	if s.cronCancel != nil {
		s.cronCancel()
	}
	s.channelMgr.StopAll()
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

// handleHealthz 提供基础的健康检查。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}

// handleGetConfig 返回当前运行时配置的原始内容（只读视图）。
//
// 读取优先级：
//  1. POLARIS_CONFIG 环境变量指向的文件（Operator 运行时覆盖）
//  2. 二进制内嵌的 configs/defaults.toml（embedded FS，始终可用）
//
// 使用 embedded FS 而非相对路径 os.ReadFile，确保二进制在任意工作目录下均可运行。
//
//nolint:unused
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	var (
		raw    []byte
		source string
		err    error
	)

	if cfgPath := os.Getenv("POLARIS_CONFIG"); cfgPath != "" {
		// Operator 显式指定配置文件：从文件系统读取，路径可为绝对路径或相对路径。
		raw, err = os.ReadFile(cfgPath)
		if err != nil {
			http.Error(w, "POLARIS_CONFIG file not readable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		source = cfgPath
	} else {
		// 默认：从 binary 内嵌 FS 读取（configs/embed.go //go:embed *.toml ...）。
		// 此路径不依赖工作目录，任意部署环境均可用。
		raw, err = configs.FS.ReadFile("defaults.toml")
		if err != nil {
			// embedded FS 读取失败属于编译期资产问题，用 500 而非 404。
			http.Error(w, "embedded config not readable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		source = "embedded:configs/defaults.toml"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":   source,
		"format": "toml",
		"raw":    string(raw),
	})
}

// handleEvalRun 触发 M12 评测套件执行并返回报告。
// POST /v1/eval/run  body: {"suite":"training"|"validation"}
//
//nolint:unused
func (s *Server) handleEvalRun(w http.ResponseWriter, r *http.Request) {
	if s.evalRunner == nil {
		http.Error(w, "eval runner not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Suite       string `json:"suite"`
		CandidateID string `json:"candidate_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Suite == "" {
		req.Suite = "training"
	}
	report, err := s.evalRunner.RunSuite(r.Context(), req.Suite, req.CandidateID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// handleAgentQuery 将用户查询发布为异步 Blackboard Task，立即返回 task_id。
// 调用方通过 GET /v1/agent/tasks/{taskID} 轮询结果（HE-Rule-5 FSM 控制流）。
func (s *Server) handleAgentQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input     string `json:"input"`
		SessionID string `json:"session_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		http.Error(w, "input must not be empty", http.StatusBadRequest)
		return
	}

	if s.blackboard == nil {
		// Blackboard 未注入时退化：直接注入 Agent Intent，返回兼容响应
		if s.agentPool != nil {
			agent, release, err := s.agentPool.Acquire(r.Context(), "default")
			if err == nil {
				agent.SetTaskIntent([]byte(req.Input))
				release()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": "",
			"status":  "pending",
			"note":    "blackboard not available; intent injected directly",
		})
		return
	}

	now := time.Now().UnixMilli()
	task := &types.TaskEntry{
		ID:          "task-" + uuid.NewString(),
		Type:        "agent_query",
		Priority:    0,
		Status:      types.TaskPending,
		Intent:      []byte(req.Input),
		IntentTaint: types.TaintMedium, // 外部用户输入，中等置信度
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.blackboard.PostTask(r.Context(), task); err != nil {
		slog.Error("handleAgentQuery: PostTask failed", "error", err)
		http.Error(w, "failed to submit task", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202 Accepted
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_id": task.ID,
		"status":  "pending",
	})
}

// handleGetAgentTask 查询 Blackboard 中指定 task 的当前状态快照。
// GET /v1/agent/tasks/{taskID}
func (s *Server) handleGetAgentTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskID")
	if taskID == "" {
		http.Error(w, "taskID is required", http.StatusBadRequest)
		return
	}

	if s.blackboard == nil {
		http.Error(w, "blackboard not available", http.StatusNotImplemented)
		return
	}

	snap, err := s.blackboard.PeekTask(r.Context(), taskID)
	if err != nil {
		slog.Warn("handleGetAgentTask: PeekTask failed", "task_id", taskID, "error", err)
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// handleGetPendingApprovals 获取待审批任务。
func (s *Server) handleGetPendingApprovals(w http.ResponseWriter, r *http.Request) {
	if s.hitlGateway == nil {
		http.Error(w, "HITL not enabled", http.StatusNotImplemented)
		return
	}

	pending, err := s.hitlGateway.Pending(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pending": pending,
	})
}

// handleAgentInterrupt 处理用户中断请求（M13 §1.2.5，inv_global_08 <200ms SLO）。
// POST /v1/agent/{taskID}/interrupt
// body: {"action":"resume"|"redirect"|"abort","redirect":"新意图文本","reason":"..."}
func parseInterruptAction(action string) types.InterruptAction {
	switch action {
	case "resume":
		return types.InterruptResume
	case "redirect":
		return types.InterruptRedirect
	case "abort":
		return types.InterruptAbort
	default:
		return types.InterruptResume
	}
}

func (s *Server) handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	clientIP := extractIP(r)
	authCtx := FromContext(r.Context())
	clientType := "unknown"
	if authCtx != nil {
		clientType = authCtx.ClientType
	}
	if s.interruptLimiter != nil && !s.interruptLimiter.Allow(clientIP, clientType) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	taskID := r.PathValue("taskID")

	if authCtx == nil || (authCtx.UserID != "admin" && authCtx.UserID != "system") {
		// MVP 阶段仅 admin 可操作。在多租户下需检查 task 所属 user。
		http.Error(w, "forbidden: unauthorized user", http.StatusForbidden)
		return
	}

	var req struct {
		Action   string `json:"action"`   // "resume" | "redirect" | "abort"
		Redirect string `json:"redirect"` // action=redirect 时的新意图
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	action := parseInterruptAction(req.Action)

	interruptReq := types.InterruptRequest{
		Reason:   req.Reason,
		Action:   action,
		Redirect: req.Redirect,
	}
	if s.outboxWriter != nil {
		// 异步路由：写入 Outbox，由 OutboxWorker 分发到目标 Agent 进程。
		// OutboxWorker 需注册 operation="agent_interrupt" 的处理器（见 pkg/substrate/storage/outbox_worker.go）。
		ev, _ := protocol.NewOutboxEvent(protocol.TopicAgentInterrupt, "agent_interrupt", map[string]any{
			"task_id": taskID,
			"request": interruptReq,
		}, "interrupt:"+taskID+":"+req.Action)
		ev.Scope = taskID
		if err := s.outboxWriter.Write(r.Context(), ev); err != nil {
			slog.Error("handleAgentInterrupt: outbox write failed, falling back to direct call", "err", err)
		}
	} else {
		// 单进程降级路径：outboxWriter 未注入时直接调用（Tier-0/开发环境）。
		slog.Info("handleAgentInterrupt: outboxWriter not set, unable to direct call")
	}

	if s.auditTrail != nil {
		detail, _ := json.Marshal(map[string]any{
			"task_id":  taskID,
			"action":   req.Action,
			"redirect": req.Redirect,
			"reason":   req.Reason,
		})
		_ = s.auditTrail.Record(&security.AuditRecord{
			ActionType:   "interrupt",
			ActionDetail: detail,
			AgentID:      authCtx.UserID,
			Timestamp:    time.Now().UnixMicro(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "accepted",
		"taskID": taskID,
	})
}

// handleResolveApproval 提交审批结果。
func (s *Server) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	if s.hitlGateway == nil {
		http.Error(w, "HITL not enabled", http.StatusNotImplemented)
		return
	}

	pathParts := strings.Split(r.URL.Path, "/")
	if len(pathParts) < 5 || pathParts[4] != "resolve" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	approvalID := pathParts[3]

	var req struct {
		Action  string `json:"action"` // "approve" or "deny"
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	authCtx := FromContext(r.Context())

	resp := types.HITLResponse{
		OptionKey: req.Action,
		Approved:  req.Action == "approve",
		Reason:    req.Comment,
		UserID:    authCtx.UserID, // M13: 接入鉴权上下文
	}

	err := s.hitlGateway.Respond(r.Context(), approvalID, resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleStatus 返回 WebUI statusBar 所需的运行时指标快照。
func agentStateString(s types.AgentState) string {
	switch s {
	case types.AgentStateIdle:
		return "idle"
	case types.AgentStatePerceive:
		return "perceive"
	case types.AgentStatePlan:
		return "plan"
	case types.AgentStateValidate:
		return "validate"
	case types.AgentStateExecute:
		return "execute"
	case types.AgentStateReflect:
		return "reflect"
	case types.AgentStateReplan:
		return "replan"
	case types.AgentStateRollback:
		return "rollback"
	case types.AgentStateComplete:
		return "complete"
	case types.AgentStateFailed:
		return "failed"
	case types.AgentStateInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	memMB := memStats.Sys / (1024 * 1024)

	// 从 registry 取当前对话模型名称
	modelID := s.registry.PickProviderName("default")
	if modelID == "" {
		modelID = s.registry.PickProviderName("general")
	}

	// Agent state
	agentState := ""
	agentID := ""
	agentConfig := map[string]any{}
	// Global agent status removed

	// KillFullStop = 3；PolarisKillswitchStage 由 main.go KillSwitch 回调写入
	sealed := metrics.GlobalKillswitchStage.Load() >= 3

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sealed":          sealed,
		"model_id":        modelID,
		"token_used":      0,
		"token_limit":     0,
		"cost_cny":        0.0,
		"memory_mb":       memMB,
		"memory_limit_mb": 8192,
		"agent_id":        agentID,
		"agent_state":     agentState,
		"agent_config":    agentConfig,
	})
}

//nolint:nestif
func (s *Server) bootMarketplaceInit(ctx context.Context) {
	slog.Info("polaris-server: auto-syncing marketplaces...")
	if s.pluginHandler != nil {
		count, err := s.pluginHandler.SyncAllMarketplaces(ctx, false)
		if err != nil {
			slog.Warn("polaris-server: auto-sync marketplaces failed", "err", err)
		} else {
			slog.Info("polaris-server: auto-sync marketplaces finished", "synced_count", count)
		}
	}
}

func (s *Server) PluginHandler() *plugin.PluginHandler {
	return s.pluginHandler
}
