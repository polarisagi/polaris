package sysadmin

import (
	"time"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/channelsadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/evaladmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/insightsadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/mcpadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/workflowadmin"
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"github.com/polarisagi/polaris/internal/sysmgr/updater"

	"net/http"
	"sync"

	"context"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

type ChatDispatcher interface {
	EnsureSession(ctx context.Context, sessionID string) error
	InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []types.Message, userQuery string) []types.Message
	SaveMessage(ctx context.Context, sessionID, role, content, toolCalls, reasoningContent string, toolCount int64) error
	UpdateSessionTitle(ctx context.Context, sessionID, firstMessage string) error
	TouchSession(ctx context.Context, sessionID string) error
	ListMessages(ctx context.Context, sessionID string) ([]types.Message, error)
}
type SysAdminHandler struct {
	Chat           ChatDispatcher
	DB             protocol.SQLQuerier
	SystemRepo     repo.SystemRepository
	BudgetRepo     repo.BudgetRepository
	ChannelRepo    repo.ChannelRepository
	WorkflowRepo   repo.WorkflowRepository
	Agent          protocol.AgentController
	MCPMgr         MCPManager
	Hooks          *HookRunner
	DataDir        string
	ChatRepo       protocol.ChatRepository
	ProviderRepo   protocol.ProviderRepository
	AppRepo        repo.AppRepository
	ServerAddr     string
	AutomationRepo repo.AutomationRepository
	Registry       protocol.LLMRegistry
	HITLGateway    protocol.HITL
	Blackboard     *orchestrator.SQLiteBlackboard
	PipelineOrch   *orchestrator.PipelineOrchestrator
	PatternDAGExec *orchestrator.PatternDAGExecutor
	MapReduceExec  *orchestrator.MapReduceExecutor
	ParallelExec   *orchestrator.ParallelExecutor
	SequentialExec *orchestrator.SequentialExecutor
	SwarmCoord     *orchestrator.SwarmCoordinator
	ToolExec       func(ctx context.Context, name string, args []byte) (*types.ToolResult, error)
	ChannelMgr     interface {
		protocol.ChannelFacade
		Start(channelType, channelID string, cfg map[string]any)
		Stop(channelID string)
		ExtractMessage(channelType string, body []byte, r *http.Request) protocol.ChannelMessage
	}
	StreamIdleTimeout  time.Duration
	TemplateCacheMap   *sync.Map
	HTTPClient         *http.Client
	InstallMgr         ExtensionInstaller
	ExtRepo            protocol.ExtensionRepository
	PromptMgr          protocol.PromptFacade
	SoulMDContent      *string
	Updater            *updater.Manager
	Catalog            ToolCatalog
	NewWorktreeManager func(workingDir, worktreeRoot string) WorktreeManager
	SkillReg           protocol.SkillRegistry
	SkillSignKey       []byte
	LastEventOffset    int64

	Embedder search.Embedder

	Insights *insightsadmin.InsightsAdmin
	Cron     *cronadmin.CronAdmin
	Workflow *workflowadmin.WorkflowAdmin
	Channels *channelsadmin.ChannelsAdmin
	MCP      *mcpadmin.MCPAdmin
	// Eval 与其余子结构体不同：不在 NewSysAdminHandler 内构造（EvalStore/
	// MetaEvalSentinel 来自 AgentBundle，boot_agent.go 晚于本构造函数运行），
	// 采用与 ToolExec/InstallMgr 相同的"先 nil、Server.SetEvalAdmin 后置回填"模式。
	Eval *evaladmin.EvalAdmin
}

type Dependencies struct {
	DB             protocol.SQLQuerier
	SystemRepo     repo.SystemRepository
	BudgetRepo     repo.BudgetRepository
	ChannelRepo    repo.ChannelRepository
	WorkflowRepo   repo.WorkflowRepository
	AgentPool      protocol.AgentPool
	MCPMgr         MCPManager
	Hooks          *HookRunner
	DataDir        string
	ChatRepo       protocol.ChatRepository
	ProviderRepo   protocol.ProviderRepository
	AppRepo        repo.AppRepository
	ServerAddr     string
	AutomationRepo repo.AutomationRepository
	Chat           ChatDispatcher
	Registry       protocol.LLMRegistry
	HTTPClient     *http.Client
	ExtRepo        protocol.ExtensionRepository
	HITLGateway    protocol.HITL
	Blackboard     *orchestrator.SQLiteBlackboard
	PipelineOrch   *orchestrator.PipelineOrchestrator
	PatternDAGExec *orchestrator.PatternDAGExecutor
	MapReduceExec  *orchestrator.MapReduceExecutor
	ParallelExec   *orchestrator.ParallelExecutor
	SequentialExec *orchestrator.SequentialExecutor
	SwarmCoord     *orchestrator.SwarmCoordinator
	ChannelMgr     interface {
		protocol.ChannelFacade
		Start(channelType, channelID string, cfg map[string]any)
		Stop(channelID string)
		ExtractMessage(channelType string, body []byte, r *http.Request) protocol.ChannelMessage
	}
	StreamIdleTimeout time.Duration
}

// NewSysAdminHandler 故意不做构造函数级 fail-closed nil 强制校验——本包已有
// handler_wiring_test.go 依赖"仅传 DB 字段部分构造"的测试模式，强制校验会
// 直接破坏该回归测试。完整结论见 chat.NewChatHandler 文档注释 +
// local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md：
// HTTP 路径有 PanicRecovery 中间件兜底，真正的进程级崩溃风险在 cronadmin 后台
// goroutine（已修复为 concurrent.SafeGo），非构造函数。
func NewSysAdminHandler(deps Dependencies) *SysAdminHandler {
	h := &SysAdminHandler{
		DB:                deps.DB,
		SystemRepo:        deps.SystemRepo,
		BudgetRepo:        deps.BudgetRepo,
		ChannelRepo:       deps.ChannelRepo,
		WorkflowRepo:      deps.WorkflowRepo,
		MCPMgr:            deps.MCPMgr,
		Hooks:             deps.Hooks,
		DataDir:           deps.DataDir,
		ChatRepo:          deps.ChatRepo,
		ProviderRepo:      deps.ProviderRepo,
		AppRepo:           deps.AppRepo,
		ServerAddr:        deps.ServerAddr,
		AutomationRepo:    deps.AutomationRepo,
		Chat:              deps.Chat,
		Registry:          deps.Registry,
		HTTPClient:        deps.HTTPClient,
		ExtRepo:           deps.ExtRepo,
		HITLGateway:       deps.HITLGateway,
		PipelineOrch:      deps.PipelineOrch,
		PatternDAGExec:    deps.PatternDAGExec,
		MapReduceExec:     deps.MapReduceExec,
		ParallelExec:      deps.ParallelExec,
		SequentialExec:    deps.SequentialExec,
		SwarmCoord:        deps.SwarmCoord,
		ChannelMgr:        deps.ChannelMgr,
		StreamIdleTimeout: deps.StreamIdleTimeout,
		Insights:          insightsadmin.NewInsightsAdmin(deps.DB),
		// Store/Sentinel 均先 nil 构造（此时 AgentBundle 尚未构建），Server.SetEvalAdmin
		// 后置回填时对本对象的字段做原地赋值而非替换整个指针——server_routes.go 注册路由
		// 时捕获的是 h.Eval 这个指针本身，必须保持稳定，否则回填对已注册路由不可见
		// （与 mcpadmin.InstallMgr"先nil、SetInstallManager 原地回填"是同一模式）。
		Eval: evaladmin.NewEvalAdmin(nil, nil),
	}
	// 2026-07-07 R7 瘦身：workflow.go（原 730 行）拆为独立 workflowadmin 子包
	// （沿用 cronadmin/insightsadmin 模式）。CronTickWorkflows 回调改指向
	// h.Workflow 而非 h 自身方法。
	h.Workflow = workflowadmin.NewWorkflowAdmin(
		deps.DB,
		deps.WorkflowRepo,
		deps.AgentPool,
		deps.Chat,
		deps.Blackboard,
		nil,
		h.BuildToolSchemas,
	)
	// 2026-07-07 修复：此前 buildToolSchemas/cronTickWorkflows 两个回调硬编码为
	// nil——cron_scheduler.go 的 cronTick() 无条件调用 ca.CronTickWorkflows(ctx)、
	// cron_runner.go 的 executeAutomation() 无条件调用 ca.BuildToolSchemas()，
	// 一旦调度器被启动，首次 tick / 首次执行 automation 就会因调用 nil func 触发
	// panic。改为两阶段构造：先建好 h（含 h.Workflow），再用 h/h.Workflow 自身
	// 方法作为闭包传入 Cron，与 ToolExec/NewWorktreeManager 现有的"先 nil、
	// server_core.go 里后置回填"模式不同——这两个回调没有外部后置回填的调用点，
	// 必须在此处一次性接好。
	h.Cron = cronadmin.NewCronAdmin(
		deps.DB,
		deps.AgentPool,
		deps.AutomationRepo,
		deps.Chat,
		deps.ChannelMgr,
		deps.HITLGateway,
		deps.HTTPClient,
		deps.Registry,
		&sync.Map{},
		nil, nil,
		h.BuildToolSchemas,
		h.Workflow.CronTickWorkflows,
	)
	// 2026-07-07 R7 瘦身：channels.go（原 579 行）拆为独立 channelsadmin 子包。
	// Channels.Cron 接的是 h.Cron 本身（而非回调），用于 webhook 收到消息后顺带
	// 触发绑定该频道的 automation（见 channelsadmin/webhook_receive.go）。
	h.Channels = channelsadmin.NewChannelsAdmin(
		deps.DB,
		deps.ChannelRepo,
		deps.ChannelMgr,
		deps.Registry,
		deps.Chat,
		deps.Hooks,
		h.Cron,
		deps.AgentPool,
		nil,
		h.BuildToolSchemas,
	)
	// 2026-07-07 R7 瘦身：mcp_servers.go（原 411 行）拆为独立 mcpadmin 子包。
	// InstallMgr 与 ToolExec/Catalog 同属"先 nil、server_core.go 里后置回填"
	// 模式（SetInstallManager 里会同时回填 h.InstallMgr 和 h.MCP.InstallMgr）。
	h.MCP = mcpadmin.NewMCPAdmin(
		deps.DB,
		deps.MCPMgr,
		deps.SystemRepo,
		nil,
		deps.ExtRepo,
		deps.DataDir,
		h.ClearToolSchemaCache,
	)
	return h
}

func (h *SysAdminHandler) ExecuteTool(ctx context.Context, name string, args []byte) (*types.ToolResult, error) {
	if h.ToolExec != nil {
		return h.ToolExec(ctx, name, args)
	}
	return nil, nil
}

func (h *SysAdminHandler) HandleInsights(w http.ResponseWriter, r *http.Request) {
	h.Insights.HandleInsights(w, r)
}
