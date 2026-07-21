package server

import (
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/gateway/server/plugin"
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/security/taint"

	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"

	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"golang.org/x/time/rate"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/credential"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// ============================================================================
// NewServer 构造函数 + Start/Shutdown 生命周期管理（R7 拆分自 server_core.go）。
// Server 结构体定义 + Set* 依赖注入方法见 server_core.go。
// ============================================================================

// NewServer 创建并初始化 HTTP 网关服务器。
//
// 参数说明：
//   - rwDB  : 读写连接池（*sql.DB），供 repo 层执行 INSERT/UPDATE/DELETE 使用。
//   - db    : 只读连接池（protocol.SQLQuerier），供 handler 层执行轻量 SELECT 查询使用。
//     两个连接指向同一个 SQLite 文件，分别以 WAL 读写模式 / 只读模式打开，
//     避免批量写（如 SeedMarketplace）占住唯一写连接而阻塞读路径。
func NewServer(addr string, dataDir string, agentPool protocol.AgentPool, bb protocol.Blackboard, hitlGateway protocol.HITL, rwDB *sql.DB, db protocol.SQLQuerier, registry protocol.LLMRegistry, httpClient *http.Client, safeDialer protocol.SafeDialer, compressorCfg config.CompressorConfig, agentCfg config.AgentConfig, tbr *metrics.TokenBurnRate, rateLimiter *rate.Limiter) *Server {
	tDir := filepath.Join(dataDir, "sessions")
	// 启动时异步清理 30 天前的 transcript
	concurrent.SafeGo(context.Background(), "gateway.server.prune_transcripts", func(context.Context) {
		chat.PruneTranscripts(tDir, 30)
	})

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
	if rwDB == nil {
		panic("NewServer: rwDB（读写连接）不能为 nil")
	}
	// 必须用 NewVaultInDir(dataDir) 而非 NewVault()：后者硬编码 ~/.polarisagi/polaris，
	// 一旦运行时数据根目录被 POLARIS_DATA_DIR / cfg.System.DataDir 覆盖（Docker 部署下
	// $HOME 往往不是持久化卷），vault.key 就会和 SQLite 数据库落在不同位置，
	// 容器重启后 key 丢失导致已加密的 Provider API Key 全部无法解密。
	vault, err := credential.NewVaultInDir(dataDir)
	if err != nil {
		panic(fmt.Sprintf("NewServer: 初始化 credential vault 失败: %v", err))
	}
	// 所有 repo 均使用读写连接（rwDB），确保 SeedMarketplace / Create / Update / Delete
	// 等写操作能正常执行，不会因为只读连接而报 "attempt to write a readonly database"。
	s.chatRepo = repo.NewSQLiteChatRepository(rwDB)
	s.providerRepo = repo.NewSQLiteProviderRepository(rwDB).WithVault(vault)
	s.extRepo = repo.NewSQLiteExtensionRepository(rwDB)
	s.budgetRepo = repo.NewSQLiteBudgetRepository(rwDB)
	s.systemRepo = repo.NewSQLiteSystemRepository(rwDB)
	s.channelRepo = repo.NewSQLiteChannelRepository(rwDB)
	s.automationRepo = repo.NewSQLiteAutomationRepository(rwDB)
	s.workflowRepo = repo.NewSQLiteWorkflowRepository(rwDB)
	s.appRepo = repo.NewSQLiteAppRepository(rwDB)

	// 注入内置的 yaml 配置作为种子数据到数据库（SSoT 架构）

	// 系统提示词模板的初始化将推迟到 Setprotocol.PromptFacade 阶段，
	// 以便使用 promptMgr 提供的内嵌文件系统能力，避免模块循环依赖。

	// global agent memory setup is removed as agent is now per-session

	// 注入 embedded FS 和运行时配置目录到 memory 包（三层提示词加载的 Layer 0/1）
	// 必须在 LoadSoulMD / DefaultIdentity 之前完成
	// 注入 embedded FS 和运行时配置目录到 memory 包（三层提示词加载的 Layer 0/1）
	// 改为通过 Setprotocol.PromptFacade 注入
	//
	//
	//
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

	s.providerHandler = provider.NewProviderHandler(provider.Dependencies{
		ProviderRepo: s.providerRepo,
		ExtRepo:      s.extRepo,
		Registry:     s.registry,
		HTTPClient:   httpClient,
		TBR:          tbr,
		DB:           db,
	})
	// Use chat.WriteSSE directly
	// STT/TTS 原子指针：启动时持有 nil 引擎，InitSTTEngine/InitTTSEngine 完成后原子替换为真实引擎。
	// 必须非 nil，否则 .Store()/.Load() 调用时 nil pointer dereference。
	sttPtr := new(atomic.Pointer[chat.STTEngineBox])
	ttsPtr := new(atomic.Pointer[chat.TTSProviderBox])
	s.chatHandler = chat.NewChatHandler(chat.Dependencies{
		DB:                    db,
		ChatRepo:              s.chatRepo,
		ProviderRepo:          s.providerRepo,
		SystemRepo:            s.systemRepo,
		AgentPool:             agentPool,
		Blackboard:            bb,
		Compressor:            s.compressor,
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
		WriteSSE:              chat.WriteSSE,
		// WithWorkDir 2026-07-21 deadcode 审查修复：此前未传，@file 引用解析退化为
		// 相对进程 CWD（而非 dataDir）解析路径；同一 Dependencies 结构体的其他字段
		// 早已能拿到 s.dataDir，此处透传而非发明新配置源。
		ContextRefExpander: authcontext.NewContextRefExpander(httpClient, authcontext.WithWorkDir(s.dataDir)),
		EnableFSMChatPath:  agentCfg.EnableFSMChatPath,
		TaintTracker:       taint.NewTaintTracker(), // [W-2-C] 接入 TaintTracker
	})
	s.sysadminHandler = sysadmin.NewSysAdminHandler(sysadmin.Dependencies{
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
		HITLGateway:    hitlGateway,
		AgentPool:      s.agentPool,
		// 类型断言而非直接透传：NewServer 只接受 protocol.Blackboard 接口（见 bb 形参），
		// sysadmin/workflowadmin 需要具体类型 *orchestrator.SQLiteBlackboard 才能构造
		// StateGraphExecutor（NewStateGraphExecutor 签名要求具体类型，非接口）。生产环境
		// 唯一构造点 orchestrator.NewSQLiteBlackboard（boot_agent.go）恒满足此断言；
		// 非该类型时退化为 nil（双返回值形式，不 panic），RunStepWorkerLoop 对 nil
		// Blackboard 有显式判空处理。
		Blackboard:        blackboardConcrete(bb),
		StreamIdleTimeout: time.Duration(config.DefaultThresholds().M1Router.SafecallStreamIdleTimeoutSec) * time.Second,
	})
	s.pluginHandler = plugin.NewPluginHandler(plugin.Dependencies{
		ExtRepo:              s.extRepo,
		DB:                   db,
		HTTPClient:           httpClient,
		HITLGateway:          hitlGateway,
		DataDir:              dataDir,
		ClearToolSchemaCache: s.sysadminHandler.ClearToolSchemaCache,
		StartMCPServer:       s.sysadminHandler.MCP.StartMCPServerCtx,
	})
	mux := http.NewServeMux()

	s.registerRoutes(mux)
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

	s.isReady.Store(false)
	return s
}

// blackboardConcrete 从 protocol.Blackboard 接口安全还原具体类型
// *orchestrator.SQLiteBlackboard（双返回值断言，非该类型时返回 nil 而非 panic）。
func blackboardConcrete(bb protocol.Blackboard) *orchestrator.SQLiteBlackboard {
	concrete, _ := bb.(*orchestrator.SQLiteBlackboard)
	return concrete
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

	concurrent.SafeGo(context.Background(), "gateway.server.http_serve", func(context.Context) {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("polaris-server: serve error", "err", err)
		}
	})
	// 启动所有已配置平台的 poller
	concurrent.SafeGo(context.Background(), "gateway.server.restore_channels", func(context.Context) {
		s.channelMgr.RestoreChannelsFromDB(s.db)
	})

	// Cron runner 使用可取消 context，Shutdown 时能优雅停止。
	// 2026-07-07 修复：此调用此前被注释掉，导致 cron_schedule/event 触发的
	// automation 从未在生产环境真正运行过（HTTP 手动 /trigger 接口不受影响）；
	// Shutdown() 里 s.cronCancel() 的调用一直存在但从无对应的赋值，此为该缺口的
	// 另一半修复（见 sysadmin/handler.go NewSysAdminHandler 对应的回调 nil 修复）。
	if s.sysadminHandler != nil && s.sysadminHandler.Cron != nil {
		var cronCtx context.Context
		cronCtx, s.cronCancel = context.WithCancel(context.Background())
		s.sysadminHandler.Cron.StartCronRunner(cronCtx)
	}

	// workflow_step 自订阅 Worker（2026-07-12 StateGraphExecutor workflow 接入）：
	// Blackboard 为 nil 时（如 protocol.Blackboard 具体实现非 *orchestrator.
	// SQLiteBlackboard 的边缘场景）静默跳过，不阻塞服务器启动——workflow 触发接口
	// 仍可用，只是任务会持续 Pending 直至 Worker 可用（fail-closed 但不 fail-hard）。
	if s.sysadminHandler != nil && s.sysadminHandler.Workflow != nil && s.sysadminHandler.Workflow.Blackboard != nil {
		var workflowWorkerCtx context.Context
		workflowWorkerCtx, s.workflowStepWorkerCancel = context.WithCancel(context.Background())
		concurrent.SafeGo(workflowWorkerCtx, "gateway.sysadmin.workflow_step_worker", func(ctx context.Context) {
			if err := s.sysadminHandler.Workflow.RunStepWorkerLoop(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("workflow step worker: ListenLoop exited with error", "err", err)
			}
		})
	}

	concurrent.SafeGo(context.Background(), "gateway.server.boot_marketplace_init", func(ctx context.Context) {
		s.bootMarketplaceInit(ctx)
	})

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

	s.isReady.Store(true)
	return nil
}

// Shutdown 优雅关闭服务器。
func (s *Server) Shutdown(ctx context.Context) error {
	// 停止 Cron runner（释放已排队任务 goroutine，拒绝新触发）
	if s.cronCancel != nil {
		s.cronCancel()
	}
	if s.workflowStepWorkerCancel != nil {
		s.workflowStepWorkerCancel()
	}
	s.channelMgr.StopAll()
	if s.srv != nil {
		// [修复] 原 fmt.Errorf("...: %w", s.srv.Shutdown(ctx)) 无论 Shutdown 是否成功
		// 都会返回非 nil error（%w 包裹 nil 仍构造出非 nil *errors.errorString 对象）——
		// 优雅关闭路径恒报错，调用方日志会把每次正常关闭都误判为失败。
		if err := s.srv.Shutdown(ctx); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "server shutdown failed", err)
		}
	}
	return nil
}
