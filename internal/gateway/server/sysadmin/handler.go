package sysadmin

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"github.com/polarisagi/polaris/internal/sysmgr/updater"

	"github.com/polarisagi/polaris/internal/prompt"

	"github.com/polarisagi/polaris/internal/extension/marketplace"

	"github.com/polarisagi/polaris/internal/channel/adapter"

	"github.com/polarisagi/polaris/internal/llm"

	"net/http"
	"sync"

	"context"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type ChatDispatcher interface {
	EnsureSession(ctx context.Context, sessionID string) error
	InjectSystemPrompt(ctx context.Context, history []types.Message) []types.Message
	SaveMessage(ctx context.Context, sessionID, role, content, toolCalls string, toolCount int64) error
	UpdateSessionTitle(ctx context.Context, sessionID, firstMessage string) error
	TouchSession(ctx context.Context, sessionID string) error
	LoadMessages(ctx context.Context, sessionID string) ([]types.Message, error)
	GenerateReply(ctx context.Context, req *types.InferRequest, sessionID string) (string, error)
	RunPostProcessors(ctx context.Context, sessionID, reply string)
}
type SysAdminHandler struct {
	Chat           ChatDispatcher
	DB             protocol.SQLQuerier
	SystemRepo     repo.SystemRepository
	BudgetRepo     repo.BudgetRepository
	ChannelRepo    repo.ChannelRepository
	WorkflowRepo   repo.WorkflowRepository
	Agent          protocol.AgentController
	MCPMgr         *mcp.MCPManager
	Hooks          *HookRunner
	DataDir        string
	ChatRepo       protocol.ChatRepository
	ProviderRepo   protocol.ProviderRepository
	AppRepo        repo.AppRepository
	ServerAddr     string
	AutomationRepo repo.AutomationRepository
	Registry       *llm.ProviderRegistry
	HITLGateway    protocol.HITL
	ToolExec       func(ctx context.Context, name string, args []byte) (*types.ToolResult, error)
	ChannelMgr     interface {
		SendReply(ctx context.Context, channelID string, replyTo string, options map[string]any, srcMsg adapter.Message, replyText string)
		Start(channelType, channelID string, cfg map[string]any)
		Stop(channelID string)
	}
	TemplateCacheMap *sync.Map
	HTTPClient       *http.Client
	InstallMgr       *marketplace.Manager
	ExtRepo          protocol.ExtensionRepository
	PromptMgr        *prompt.Manager
	SoulMDContent    *string
	Updater          *updater.Manager
	ToolReg          protocol.ToolRegistry
	ToolSchemaCache  []types.ToolSchema
	ToolSchemaMu     sync.RWMutex
	SkillReg         protocol.SkillRegistry
	SkillSignKey     []byte
	LastEventOffset  int64
}

func NewSysAdminHandler(
	db protocol.SQLQuerier,
	systemRepo repo.SystemRepository,
	budgetRepo repo.BudgetRepository,
	channelRepo repo.ChannelRepository,
	workflowRepo repo.WorkflowRepository,
	agent protocol.AgentController,
	mcpMgr *mcp.MCPManager,
	hooks *HookRunner,
	dataDir string,
	chatRepo protocol.ChatRepository,
	providerRepo protocol.ProviderRepository,
	appRepo repo.AppRepository,
	serverAddr string,
	automationRepo repo.AutomationRepository,
) *SysAdminHandler {
	return &SysAdminHandler{
		DB:             db,
		SystemRepo:     systemRepo,
		BudgetRepo:     budgetRepo,
		ChannelRepo:    channelRepo,
		WorkflowRepo:   workflowRepo,
		Agent:          agent,
		MCPMgr:         mcpMgr,
		Hooks:          hooks,
		DataDir:        dataDir,
		ChatRepo:       chatRepo,
		ProviderRepo:   providerRepo,
		AppRepo:        appRepo,
		ServerAddr:     serverAddr,
		AutomationRepo: automationRepo,
	}
}

func (h *SysAdminHandler) ExecuteTool(ctx context.Context, name string, args []byte) (*types.ToolResult, error) {
	if h.ToolExec != nil {
		return h.ToolExec(ctx, name, args)
	}
	return nil, nil
}
