package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/cognition/kernel"
	"github.com/polarisagi/polaris/pkg/cognition/memory"
	"github.com/polarisagi/polaris/pkg/extensions/marketplace"
	"github.com/polarisagi/polaris/pkg/extensions/mcp"
	"github.com/polarisagi/polaris/pkg/gateway/channels"
	"github.com/polarisagi/polaris/pkg/substrate/inference"
	"github.com/polarisagi/polaris/pkg/substrate/inference/stt"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
	"github.com/polarisagi/polaris/pkg/substrate/updater"
	webui "github.com/polarisagi/polaris/web"

	"gopkg.in/yaml.v3"
)

// Server 包装 HTTP 与 WebSocket 服务，作为 M13 的对外网关。
type Server struct {
	addr            string
	srv             *http.Server
	agent           *kernel.Agent
	blackboard      protocol.Blackboard
	hitlGateway     protocol.HITL
	db              *sql.DB
	registry        *inference.ProviderRegistry                                                       // 热重载 Provider 注册表
	httpClient      *http.Client                                                                      // 复用 SafeHTTPClient
	transcriptDir   string                                                                            // per-session JSONL transcript 目录
	hooks           *HookRunner                                                                       // Shell Script Hooks（End-User 扩展点）
	compressor      *Compressor                                                                       // 上下文超长自动压缩
	channelMgr      *channels.Manager                                                                 // 所有聊天平台 poller 管理
	mcpMgr          *mcp.MCPManager                                                                   // MCP Server 连接管理
	toolReg         protocol.ToolRegistry                                                             // builtin tool 元数据
	skillReg        protocol.SkillRegistry                                                            // skill 元数据
	toolExec        func(ctx context.Context, name string, args []byte) (*protocol.ToolResult, error) // tool_use 执行器
	logStore        *LogStore                                                                         // 日志环形缓冲 + SSE 广播
	evalRunner      protocol.EvalRunner                                                               // M12 评测套件
	dataDir         string                                                                            // 项目统一的数据根目录
	installMgr      *marketplace.Manager
	scriptRunner    marketplace.HookRunner // install hook 沙箱执行器（ContainerSandbox.RunScript）
	skillSignKey    []byte
	toolSchemaCache []protocol.ToolSchema
	toolSchemaMu    sync.RWMutex

	updater *updater.Manager // OTA 自更新管理器（可为 nil）

	// 系统提示词组装缓存（启动时一次性加载，运行期不变）
	soulMDContent  string // ~/.polarisagi/polaris/config/SOUL.md 内容
	serverPlatform string // 接入平台标识，决定平台感知提示词（cli/webui/api/cron）

	// M9 激活的系统提示词（从 DB prompt_versions 表读取，Activate 回调热更新）
	activatedSystemPromptMu sync.RWMutex
	activatedSystemPrompt   string // task_type='general' 的激活版本

	// Cron runner 生命周期控制
	cronCancel context.CancelFunc
}

func (s *Server) SetInstallManager(m *marketplace.Manager)  { s.installMgr = m }
func (s *Server) SetScriptRunner(r marketplace.HookRunner)  { s.scriptRunner = r }
func (s *Server) SetSkillSigningKey(k []byte)               { s.skillSignKey = k }
func (s *Server) SetUpdater(u *updater.Manager)             { s.updater = u }

// SetMCPManager 注入 MCPManager（NewServer 之后、Start 之前调用）。
// 同时注册缓存失效回调：异步插件 MCP 连接完成时自动清除 toolSchemaCache，
// 确保 LLM 在下次推理时能看到新工具，而非返回连接前构建的过期缓存。
func (s *Server) SetMCPManager(m *mcp.MCPManager) {
	s.mcpMgr = m
	m.SetOnToolsChanged(s.clearToolSchemaCache)
}

// SetToolRegistry 注入 ToolRegistry（NewServer 之后、Start 之前调用）。
func (s *Server) SetToolRegistry(r protocol.ToolRegistry) { s.toolReg = r }

// SetSkillRegistry 注入 SkillRegistry（NewServer 之后、Start 之前调用）。
func (s *Server) SetSkillRegistry(r protocol.SkillRegistry) { s.skillReg = r }

// SetToolExecutor 注入工具执行函数，用于 tool_use 循环（NewServer 之后、Start 之前调用）。
func (s *Server) SetToolExecutor(fn func(ctx context.Context, name string, args []byte) (*protocol.ToolResult, error)) {
	s.toolExec = fn
}

// SetLogStore 注入日志存储（NewServer 之后、Start 之前调用）。
func (s *Server) SetLogStore(ls *LogStore) { s.logStore = ls }

// SetEvalRunner 注入 M12 评测套件（NewServer 之后、Start 之前调用）。
func (s *Server) SetEvalRunner(r protocol.EvalRunner) { s.evalRunner = r }

// buildToolSchemas 收集全部可用工具 schema，用于注入 InferRequest.Tools。
func (s *Server) buildToolSchemas() []protocol.ToolSchema { //nolint:nestif
	s.toolSchemaMu.RLock()
	if len(s.toolSchemaCache) > 0 {
		cache := s.toolSchemaCache
		s.toolSchemaMu.RUnlock()
		return cache
	}
	s.toolSchemaMu.RUnlock()

	var schemas []protocol.ToolSchema
	if s.toolReg != nil {
		for _, t := range s.toolReg.List() {
			schemas = append(schemas, protocol.ToolSchema{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			})
		}
	}
	if s.mcpMgr != nil {
		schemas = append(schemas, s.mcpMgr.ListToolSchemas()...)
	}
	// script runtime 技能：以 "skill__{slug}" 工具形式暴露给 LLM。
	// DB 存储格式为 "skill:{slug}"（内部名称），此处剥离前缀后加 skill__ 前缀生成合法的 LLM 工具名。
	if s.db != nil { //nolint:nestif
		rows, err := s.db.QueryContext(context.Background(),
			`SELECT name, capabilities FROM skills WHERE runtime='script' AND exec_mode='tool' AND deprecated=0`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var dbName, capsRaw string
				if rows.Scan(&dbName, &capsRaw) != nil {
					continue
				}
				slug, ok := strings.CutPrefix(dbName, "skill:")
				if !ok || slug == "" {
					continue
				}
				var caps []string
				_ = json.Unmarshal([]byte(capsRaw), &caps)
				desc := ""
				for _, c := range caps {
					if d, ok := strings.CutPrefix(c, "description:"); ok {
						desc = d
						break
					}
				}
				schemas = append(schemas, protocol.ToolSchema{
					Name:        "skill__" + slug,
					Description: desc,
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"input": map[string]any{"type": "string", "description": "任务描述或输入内容"},
						},
						"required": []string{"input"},
					},
				})
			}
		}
	}

	s.toolSchemaMu.Lock()
	s.toolSchemaCache = schemas
	s.toolSchemaMu.Unlock()
	return schemas
}

func (s *Server) clearToolSchemaCache() {
	s.toolSchemaMu.Lock()
	s.toolSchemaCache = nil
	s.toolSchemaMu.Unlock()
}

// NewServer 创建新的 HTTP Server。
// DEV_MODE=1 时将静态资源请求反向代理到 Vite dev server (:5173)。
func NewServer(addr string, dataDir string, agent *kernel.Agent, bb protocol.Blackboard, hitlGateway protocol.HITL, db *sql.DB, registry *inference.ProviderRegistry, httpClient *http.Client, safeDialer protocol.SafeDialer) *Server {
	tDir := filepath.Join(dataDir, "sessions")
	go PruneTranscripts(tDir, 30) // 启动时异步清理 30 天前的 transcript

	s := &Server{
		addr:          addr,
		agent:         agent,
		blackboard:    bb,
		hitlGateway:   hitlGateway,
		db:            db,
		registry:      registry,
		httpClient:    httpClient,
		transcriptDir: tDir,
		hooks:         NewHookRunner(dataDir),
		dataDir:       dataDir,
	}

	// 注入内置的 yaml 配置作为种子数据到数据库（SSoT 架构）
	seedBuiltinConfig(db)

	prefs, _ := LoadAllPreferences(context.Background(), db)
	sysTmpl, ok := prefs["system_prompt_template"]
	if !ok {
		if b, err := os.ReadFile("configs/prompts/system_prompt.md"); err == nil {
			sysTmpl = string(b)
			_, _ = db.Exec("INSERT INTO preferences(key, value) VALUES('system_prompt_template', ?)", sysTmpl)
			prefs["system_prompt_template"] = sysTmpl
		} else {
			sysTmpl = "你是 {{.AgentName}}，{{.AgentRole}}。\n当前运行模型：{{.ModelID}}。"
		}
	}

	if agent != nil && agent.Memory() != nil {
		if ic, ok := agent.Memory().Working().Immutable().(*memory.ImmutableCore); ok {
			ic.SystemPromptTemplate = sysTmpl
			if goal, hasGoal := prefs["global_goal"]; hasGoal {
				ic.GlobalGoal = goal
			} else if legacyGoal, hasLegacy := prefs["system_prompt"]; hasLegacy {
				ic.GlobalGoal = legacyGoal
			}
		}
	}

	// 注入 embedded FS 和运行时配置目录到 memory 包（三层提示词加载的 Layer 0/1）
	// 必须在 LoadSoulMD / DefaultIdentity 之前完成
	memory.SetEmbeddedPrompts(configs.FS)
	memory.SetConfigDir(filepath.Join(dataDir, "config"))

	// 加载用户身份（三层优先级：user prompts/identity.md > SOUL.md > embedded default）
	s.soulMDContent = memory.LoadSoulMD()

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

	s.compressor = newCompressor(db, s.hooks)
	s.channelMgr = channels.NewManager(httpClient, func(channelType, channelID string, cfg map[string]any, msg channels.Message) {
		s.dispatchChannelMessage(channelType, channelID, cfg, msg)
	}, channels.WithSafeDialer(safeDialer))

	mux := http.NewServeMux()

	// API 端点
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleHealthz)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/doctor", s.handleDoctor)
	mux.Handle("GET /metrics", observability.MetricsHandler())
	mux.HandleFunc("GET /v1/logs/stream", s.handleLogStream)
	mux.HandleFunc("POST /v1/agent/query", s.handleAgentQuery)
	mux.HandleFunc("POST /v1/agent/stream", s.handleAgentStream)
	mux.HandleFunc("POST /v1/agent/{taskID}/interrupt", s.handleAgentInterrupt) // inv_global_08 <200ms
	mux.HandleFunc("GET /v1/approvals/pending", s.handleGetPendingApprovals)
	mux.HandleFunc("POST /v1/approvals/", s.handleResolveApproval) // /v1/approvals/{id}/resolve

	// LLM 厂商配置 API
	mux.HandleFunc("GET /v1/providers", s.handleListProviders)
	mux.HandleFunc("POST /v1/providers", s.handleCreateProvider)
	mux.HandleFunc("PUT /v1/providers/{providerID}", s.handleUpdateProvider)
	mux.HandleFunc("DELETE /v1/providers/{providerID}", s.handleDeleteProvider)
	mux.HandleFunc("POST /v1/providers/{providerID}/test", s.handleTestProvider)

	// 厂商模型管理 API（两层架构：provider → models）
	mux.HandleFunc("GET /v1/providers/{providerID}/models", s.handleListModels)
	mux.HandleFunc("POST /v1/providers/{providerID}/models", s.handleCreateModel)
	mux.HandleFunc("PUT /v1/providers/{providerID}/models/{modelID}", s.handleUpdateModel)
	mux.HandleFunc("DELETE /v1/providers/{providerID}/models/{modelID}", s.handleDeleteModel)

	// 模型角色配置 API（对话模型 / 推理模型）
	mux.HandleFunc("GET /v1/config/model-roles", s.handleGetModelRoles)
	mux.HandleFunc("PUT /v1/config/model-roles", s.handleSetModelRoles)
	mux.HandleFunc("GET /v1/config", s.handleGetConfig)

	// Preferences API
	mux.HandleFunc("GET /v1/preferences", s.handleGetPreferences)
	mux.HandleFunc("PUT /v1/preferences/{key}", s.handleSetPreference)

	// 提示词管理 API（三层所有权：Layer 1 用户自定义层，读写 ~/.polarisagi/polaris/config/prompts/）
	// Layer 0（embedded 内置默认）和 Layer 2（M9 优化）不通过此 API 暴露
	mux.HandleFunc("GET /v1/config/prompts", s.handleListPrompts)
	mux.HandleFunc("GET /v1/config/prompts/{name}", s.handleGetPrompt)
	mux.HandleFunc("PUT /v1/config/prompts/{name}", s.handleSetPrompt)
	mux.HandleFunc("DELETE /v1/config/prompts/{name}", s.handleResetPrompt)

	// M12 评测 API
	mux.HandleFunc("POST /v1/eval/run", s.handleEvalRun)

	// 会话历史 API
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/{sessionID}", s.handleGetSession)
	mux.HandleFunc("DELETE /v1/sessions/{sessionID}", s.handleDeleteSession)

	// 语音识别 API
	mux.HandleFunc("POST /v1/audio/transcriptions", s.handleAudioTranscriptions)

	// VFS 通用文件上传
	mux.HandleFunc("POST /v1/workspace/upload", s.handleVFSUpload)

	// 全文搜索 API（FTS5）
	mux.HandleFunc("GET /v1/search", s.handleSearch)

	// 用量洞察 & 会话回顾
	mux.HandleFunc("GET /v1/insights", s.handleInsights)
	mux.HandleFunc("GET /v1/sessions/{sessionID}/recap", s.handleSessionRecap)

	// Trajectory 导出（自演化训练数据）
	mux.HandleFunc("GET /v1/export/trajectories", s.handleExportTrajectories)

	// 自动化 (Automations)
	mux.HandleFunc("GET /v1/automations", s.handleListAutomations)
	mux.HandleFunc("POST /v1/automations", s.handleCreateAutomation)
	mux.HandleFunc("PUT /v1/automations/{id}", s.handleUpdateAutomation)
	mux.HandleFunc("DELETE /v1/automations/{id}", s.handleDeleteAutomation)
	mux.HandleFunc("GET /v1/automations/{id}/runs", s.handleListAutomationRuns)
	mux.HandleFunc("POST /v1/automations/{id}/trigger", s.handleTriggerAutomation)
	mux.HandleFunc("GET /v1/automation-templates", s.handleListAutomationTemplates)

	// 聊天平台集成 API
	mux.HandleFunc("GET /v1/channels", s.handleListChannels)
	mux.HandleFunc("POST /v1/channels", s.handleCreateChannel)
	mux.HandleFunc("PUT /v1/channels/{channelID}", s.handleUpdateChannel)
	mux.HandleFunc("DELETE /v1/channels/{channelID}", s.handleDeleteChannel)
	mux.HandleFunc("POST /v1/webhooks/{channelType}/{channelID}", s.handleWebhookReceive)

	// 工具 & Skill 管理 API
	mux.HandleFunc("GET /v1/tools", s.handleListTools)
	mux.HandleFunc("GET /v1/tools/schemas", s.handleListToolSchemas)
	mux.HandleFunc("POST /v1/tools/{name}/execute", s.handleExecuteTool)
	mux.HandleFunc("GET /v1/skills", s.handleListSkills)
	mux.HandleFunc("POST /v1/skills/install", s.handleInstallSkill)

	// MCP Server 管理 API
	mux.HandleFunc("GET /v1/mcp-servers", s.handleListMCPServers)
	mux.HandleFunc("POST /v1/mcp-servers", s.handleCreateMCPServer)
	mux.HandleFunc("PUT /v1/mcp-servers/{serverID}", s.handleUpdateMCPServer)
	mux.HandleFunc("DELETE /v1/mcp-servers/{serverID}", s.handleDeleteMCPServer)
	mux.HandleFunc("POST /v1/mcp-servers/{serverID}/test", s.handleTestMCPServer)

	// 插件目录 API
	mux.HandleFunc("GET /v1/plugins/catalog", s.handleListPluginCatalog)
	mux.HandleFunc("POST /v1/plugins/install", s.handleInstallPlugin)
	mux.HandleFunc("DELETE /v1/plugins/{catalogID}", s.handleUninstallPlugin)

	// 已安装插件管理 API（对接 plugins 运行时表）
	mux.HandleFunc("GET /v1/plugins", s.handleListPlugins)
	mux.HandleFunc("PUT /v1/plugins/{id}", s.handleUpdatePlugin)
	mux.HandleFunc("PATCH /v1/plugins/{id}/mcp/{serverName}", s.handleTogglePluginMCP)

	// Custom Entity Creation
	mux.HandleFunc("POST /v1/mcp/create", s.handleCreateMCP)
	mux.HandleFunc("POST /v1/skills/create", s.handleCreateSkill)
	mux.HandleFunc("POST /v1/plugins/create", s.handleCreatePlugin)
	mux.HandleFunc("POST /v1/apps/create", s.handleCreateApp)

	// 插件市场 API
	mux.HandleFunc("GET /v1/plugins/marketplaces", s.handleListMarketplaces)
	mux.HandleFunc("POST /v1/plugins/marketplaces", s.handleAddMarketplace)
	mux.HandleFunc("DELETE /v1/plugins/marketplaces/{id}", s.handleDeleteMarketplace)
	mux.HandleFunc("POST /v1/plugins/sync", s.handleSyncMarketplaces)

	// OpenAI 兼容端点（允许第三方 OpenAI SDK 客户端直接对接）
	mux.HandleFunc("POST /v1/chat/completions", s.handleOpenAIChat)

	// 预算管理
	mux.HandleFunc("GET /v1/config/budget", s.handleGetBudget)
	mux.HandleFunc("PUT /v1/config/budget", s.handleSetBudget)

	// 系统备份 / 恢复
	mux.HandleFunc("GET /v1/export/backup", s.handleExportBackup)
	mux.HandleFunc("POST /v1/import/backup", s.handleImportBackup)

	// 系统版本 & OTA 热更新（前端直接调 GitHub API 检查版本，后端只负责执行更新）
	mux.HandleFunc("GET /v1/system/version", s.handleGetVersion)
	mux.HandleFunc("POST /v1/system/update", s.handleTriggerUpdate)

	s.setupWebUI(mux)

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
func seedBuiltinConfig(db *sql.DB) {
	if b, err := configs.FS.ReadFile("marketplaces.yaml"); err == nil {
		var mps []protocol.Marketplace
		if err := yaml.Unmarshal(b, &mps); err == nil {
			now := time.Now().UTC().Format(time.RFC3339)
			for _, mp := range mps {
				_, _ = db.Exec(`INSERT OR IGNORE INTO plugin_marketplaces(id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, sort_order, created_at)
				                VALUES(?,?,?,?,?,?,1,?,1,?,?)`,
					mp.ID, mp.Name, mp.Type, mp.Publisher, mp.RepoURL, mp.Description, mp.TrustTier, mp.SortOrder, now)
			}
		}
	} else {
		slog.Warn("polaris-server: configs/marketplaces.yaml load failed", "err", err)
	}

	if b, err := configs.FS.ReadFile("registry.yaml"); err == nil {
		var entries []protocol.RegistryEntry
		if err := yaml.Unmarshal(b, &entries); err == nil {
			for _, e := range entries {
				payload, _ := json.Marshal(e)
				_, _ = db.Exec(`INSERT OR IGNORE INTO extension_catalog(id, marketplace_id, type, name, description, publisher, trust_tier, url, payload)
				                VALUES(?,?,?,?,?,?,?,?,?)`,
					e.ID, "builtin", e.Type, e.Name, e.Description, e.Publisher, e.TrustTier, e.URL, string(payload))
			}
		}
	} else {
		slog.Warn("polaris-server: configs/registry.yaml load failed", "err", err)
	}
}

// InitSTTEngine 按 FeatureGate 门控初始化 STT 引擎。
// 必须在 NewServer 之后、Start 之前调用（或与 Start 并发，mock 引擎已就绪）。
// 流程：
//  1. 立即注入 mock 引擎（保证 /v1/audio/transcriptions 不返回 503）
//  2. 若门控禁用，仅打 Info 日志后返回
//  3. 否则在后台 goroutine：EnsureAssets → LoadLibrary → NewEngine → 替换为真实引擎
func InitSTTEngine(ctx context.Context, dataDir string, gate *observability.FeatureGate, httpClient *http.Client, sttConfig config.STTConfig) {
	sttDir := filepath.Join(dataDir, "models", "sensevoice")

	// 立即设置 mock 引擎，保证接口可用
	if mockEngine, err := stt.NewEngine("", ""); err == nil {
		SetSTTEngine(mockEngine)
	}

	// 门控检查
	if gate != nil && gate.State(observability.FeatureLocalSTT) == observability.FeatureDisabled {
		slog.Info("stt: FeatureLocalSTT disabled by FeatureGate, using mock engine")
		return
	}

	// 异步下载 + 重载：不阻塞启动路径
	go func() {
		if err := stt.EnsureAssets(ctx, sttDir, httpClient, sttConfig.SherpaVersion, sttConfig.SenseVoiceModelURL, sttConfig.PunctModelURL); err != nil {
			slog.Warn("stt: asset download failed, keeping mock engine", "err", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := stt.LoadLibrary(libPath); err != nil {
			slog.Warn("stt: library load failed after download, keeping mock engine", "err", err)
			return
		}

		modelDir := stt.ModelDir(sttDir)
		punctDir := ""
		if sttConfig.PunctModelURL != "" {
			punctDir = stt.PunctModelDir(sttDir)
		}
		engine, err := stt.NewEngine(modelDir, punctDir)
		if err != nil {
			slog.Warn("stt: engine init failed, keeping mock engine", "err", err)
			return
		}

		SetSTTEngine(engine)
		slog.Info("stt: real engine active (sherpa-onnx SenseVoice)", "model_dir", modelDir)
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
		return err
	}

	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("polaris-server: serve error", "err", err)
		}
	}()
	go s.channelMgr.LoadFromDB(s.db) // 启动所有已配置平台的 poller

	// Cron runner 使用可取消 context，Shutdown 时能优雅停止
	cronCtx, cronCancel := context.WithCancel(context.Background())
	s.cronCancel = cronCancel
	s.startCronRunner(cronCtx)

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
	return s.srv.Shutdown(ctx)
}

// handleHealthz 提供基础的健康检查。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
}

// handleGetConfig 返回当前运行时配置的 YAML 原始内容（只读视图）。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfgPath := os.Getenv("POLARIS_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/defaults.toml"
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		http.Error(w, "config file not readable: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":   cfgPath,
		"format": "yaml",
		"raw":    string(raw),
	})
}

// handleEvalRun 触发 M12 评测套件执行并返回报告。
// POST /v1/eval/run  body: {"suite":"training"|"validation"}
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

// handleAgentQuery 处理同步的单次对话/查询请求。
func (s *Server) handleAgentQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input     string `json:"input"`
		SessionID string `json:"session_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 阻塞等待内核执行（MVP 直接向 Agent 注入 Intent，但目前内核仅支持独占执行）
	// M8 Blackboard 集成后应该发布 Task，然后等待
	s.agent.SetTaskIntent([]byte(req.Input))
	// s.agent.SendIntent(...) 启动等

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"response": "Agent query acknowledged (async). Use SSE for streaming or Blackboard for results.",
	})
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
func (s *Server) handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		http.Error(w, "agent not available", http.StatusServiceUnavailable)
		return
	}

	taskID := r.PathValue("taskID")
	var req struct {
		Action   string `json:"action"`   // "resume" | "redirect" | "abort"
		Redirect string `json:"redirect"` // action=redirect 时的新意图
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	action := kernel.InterruptResume
	switch req.Action {
	case "redirect":
		action = kernel.InterruptRedirect
	case "abort":
		action = kernel.InterruptAbort
	}

	s.agent.Interrupt(kernel.InterruptRequest{
		Reason:   req.Reason,
		Action:   action,
		Redirect: req.Redirect,
	})

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

	resp := protocol.HITLResponse{
		OptionKey: req.Action,
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
func agentStateString(s protocol.AgentState) string {
	switch s {
	case protocol.AgentStateIdle:
		return "idle"
	case protocol.AgentStatePerceive:
		return "perceive"
	case protocol.AgentStatePlan:
		return "plan"
	case protocol.AgentStateValidate:
		return "validate"
	case protocol.AgentStateExecute:
		return "execute"
	case protocol.AgentStateReflect:
		return "reflect"
	case protocol.AgentStateReplan:
		return "replan"
	case protocol.AgentStateRollback:
		return "rollback"
	case protocol.AgentStateComplete:
		return "complete"
	case protocol.AgentStateFailed:
		return "failed"
	case protocol.AgentStateInterrupt:
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
	if s.agent != nil {
		agentID = s.agent.ID
		agentState = agentStateString(s.agent.StateMachine().Current())
		agentConfig = map[string]any{
			"max_replan":     s.agent.Config.MaxReplan,
			"default_budget": s.agent.Config.DefaultBudget,
			"max_steps":      s.agent.Config.MaxSteps,
		}
	}

	// KillFullStop = 3；PolarisKillswitchStage 由 main.go KillSwitch 回调写入
	sealed := observability.GlobalKillswitchStage.Load() >= 3

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
	if _, err := s.SyncAllMarketplaces(ctx, false); err != nil {
		slog.Error("polaris-server: auto-sync marketplaces failed", "err", err)
		return
	}
	slog.Info("polaris-server: auto-sync marketplaces finished")

}
