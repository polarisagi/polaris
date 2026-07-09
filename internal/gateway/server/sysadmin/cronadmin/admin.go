package cronadmin

import (
	"context"
	"net/http"
	"sync"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type WorktreeManager interface {
	PrepareWorktree(ctx context.Context, branchSuffix string) (wtDir string, branchName string, err error)
	CommitChanges(ctx context.Context, wtDir, branchName string) (hasChanges bool, diffSummary string, err error)
	PushBranch(ctx context.Context, wtDir, branchName string) error
	CreatePullRequest(ctx context.Context, branchName, title, body string) error
	Cleanup(wtDir string)
}

type AgentController interface {
	// Marker interface
}

type ChatDispatcher interface {
	EnsureSession(ctx context.Context, sessionID string) error
	InjectSystemPrompt(ctx context.Context, agent protocol.AgentController, history []types.Message, basePrompt string) []types.Message
	SaveMessage(ctx context.Context, sessionID, role, content, action, result string, latencyMs int64) error
	UpdateSessionTitle(ctx context.Context, sessionID string, title string) error
}

type ChannelManager interface {
	SendReply(ctx context.Context, channelID string, replyTo string, options map[string]any, srcMsg cadapter.Message, replyText string)
}

type HITLGateway interface {
	Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error)
}

type CronAdmin struct {
	DB               protocol.SQLQuerier
	Agent            protocol.AgentController
	AutomationRepo   repo.AutomationRepository
	Chat             ChatDispatcher
	ChannelMgr       ChannelManager
	HITLGateway      HITLGateway
	HTTPClient       *http.Client
	Registry         protocol.LLMRegistry
	TemplateCacheMap *sync.Map
	LastEventOffset  int64

	ToolExec           func(ctx context.Context, name string, args []byte) (*types.ToolResult, error)
	NewWorktreeManager func(workingDir, worktreeRoot string) WorktreeManager
	BuildToolSchemas   func() []types.ToolSchema
	CronTickWorkflows  func(ctx context.Context)
}

func NewCronAdmin(
	db protocol.SQLQuerier,
	agent protocol.AgentController,
	automationRepo repo.AutomationRepository,
	chat ChatDispatcher,
	channelMgr ChannelManager,
	hitlGateway HITLGateway,
	httpClient *http.Client,
	registry protocol.LLMRegistry,
	templateCacheMap *sync.Map,
	toolExec func(ctx context.Context, name string, args []byte) (*types.ToolResult, error),
	newWorktreeManager func(workingDir, worktreeRoot string) WorktreeManager,
	buildToolSchemas func() []types.ToolSchema,
	cronTickWorkflows func(ctx context.Context),
) *CronAdmin {
	return &CronAdmin{
		DB:                 db,
		Agent:              agent,
		AutomationRepo:     automationRepo,
		Chat:               chat,
		ChannelMgr:         channelMgr,
		HITLGateway:        hitlGateway,
		HTTPClient:         httpClient,
		Registry:           registry,
		TemplateCacheMap:   templateCacheMap,
		ToolExec:           toolExec,
		NewWorktreeManager: newWorktreeManager,
		BuildToolSchemas:   buildToolSchemas,
		CronTickWorkflows:  cronTickWorkflows,
	}
}
