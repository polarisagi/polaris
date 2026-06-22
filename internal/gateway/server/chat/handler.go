package chat

import (
	"github.com/polarisagi/polaris/internal/protocol/repo"

	"github.com/polarisagi/polaris/internal/extension/mcp"

	"github.com/polarisagi/polaris/internal/prompt"

	"sync"

	"github.com/polarisagi/polaris/internal/llm"

	"net/http"

	"context"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/internal/protocol"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

type SessionCompressor interface {
	Stats(msgs []apptypes.Message) types.ContextStats
	ForceCompact(ctx context.Context, sessionID string, msgs []apptypes.Message, provider protocol.Provider) ([]apptypes.Message, types.CompactResult, error)
}

type ChatHandler struct {
	DB            protocol.SQLQuerier
	ChatRepo      protocol.ChatRepository
	ProviderRepo  protocol.ProviderRepository
	SystemRepo    repo.SystemRepository
	Agent         protocol.AgentController
	Blackboard    protocol.Blackboard
	Compressor    *Compressor
	SlashRouter   *SlashCommandRouter
	TranscriptDir string
	PromptMgr     *prompt.Manager
	SoulMDContent *string
	ToolProvider  interface {
		BuildToolSchemas() []apptypes.ToolSchema
		ExecuteTool(ctx context.Context, name string, args []byte) (*apptypes.ToolResult, error)
	}

	Hooks                   HookRunner
	DataDir                 string
	Registry                *llm.ProviderRegistry
	SkillReg                protocol.SkillRegistry
	ToolReg                 protocol.ToolRegistry
	MCPMgr                  *mcp.MCPManager
	ServerPlatform          string
	BaseSystemPromptTpl     string
	ActivatedSystemPromptMu sync.RWMutex
	ActivatedSystemPrompt   string
	LogStore                interface {
		Append(entry any)
		Subscribe() chan any
		Unsubscribe(chan any)
	}

	STTEngine *atomic.Pointer[stt.Engine]
	TTSEngine *atomic.Pointer[tts.Engine]
	WriteSSE  func(http.ResponseWriter, http.Flusher, string, any)
}

func NewChatHandler(
	db protocol.SQLQuerier,
	chatRepo protocol.ChatRepository,
	providerRepo protocol.ProviderRepository,
	systemRepo repo.SystemRepository,
	agent protocol.AgentController,
	bb protocol.Blackboard,
	compressor *Compressor,
	transcriptDir string,
	sttEngine *atomic.Pointer[stt.Engine],
	ttsEngine *atomic.Pointer[tts.Engine],
	writeSSE func(http.ResponseWriter, http.Flusher, string, any),
) *ChatHandler {
	return &ChatHandler{
		DB:            db,
		ChatRepo:      chatRepo,
		ProviderRepo:  providerRepo,
		SystemRepo:    systemRepo,
		Agent:         agent,
		Blackboard:    bb,
		Compressor:    compressor,
		SlashRouter:   NewSlashCommandRouter(compressor, chatRepo, writeSSE),
		TranscriptDir: transcriptDir,
		STTEngine:     sttEngine,
		TTSEngine:     ttsEngine,
		WriteSSE:      writeSSE,
	}
}

type HookRunner interface {
	Fire(event string, env map[string]string)
	FireBefore(event string, env map[string]string) (blocked bool, reason string)
}

func (h *ChatHandler) GenerateReply(ctx context.Context, req *apptypes.InferRequest, sessionID string) (string, error) {
	// TODO: Implement
	return "", nil
}

func (h *ChatHandler) RunPostProcessors(ctx context.Context, sessionID, reply string) {
	// TODO: Implement
}
