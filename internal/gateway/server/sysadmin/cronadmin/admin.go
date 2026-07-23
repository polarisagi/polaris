package cronadmin

import (
	"context"
	"net/http"
	"sync"

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
	// SampleAndScoreReply 见 chat.ChatHandler 同名方法（M12 §9 连续采样监控
	// 写侧，2026-07-14 补齐）。
	SampleAndScoreReply(sessionID, query, response string)
}

type ChannelMgr interface {
	protocol.ChannelFacade
}

type HITLGateway interface {
	Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error)
}

type CronAdmin struct {
	DB               protocol.SQLQuerier
	AgentPool        protocol.AgentPool
	AutomationRepo   repo.AutomationRepository
	ChannelRepo      repo.ChannelRepository
	EventRepo        repo.EventRepository
	CronRepo         protocol.CronRepository
	Chat             ChatDispatcher
	ChannelMgr       ChannelMgr
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
	agentPool protocol.AgentPool,
	automationRepo repo.AutomationRepository,
	channelRepo repo.ChannelRepository,
	eventRepo repo.EventRepository,
	cronRepo protocol.CronRepository,
	chat ChatDispatcher,
	channelMgr ChannelMgr,
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
		AgentPool:          agentPool,
		AutomationRepo:     automationRepo,
		ChannelRepo:        channelRepo,
		EventRepo:          eventRepo,
		CronRepo:           cronRepo,
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
