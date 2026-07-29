package chat

import (
	"sync"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
)

type PromptAssemblyService struct {
	PromptMgr               protocol.PromptFacade
	SoulMDContent           *string
	ContextRefExpander      *authcontext.ContextRefExpander
	PersonaRefiner          *agentctx.PersonaRefiner
	BaseSystemPromptTpl     string
	ActivatedSystemPromptMu sync.RWMutex
	ActivatedSystemPrompt   string
	Registry                protocol.LLMRegistry
	ServerPlatform          string
	ToolReg                 protocol.ToolRegistry
	DB                      protocol.SQLQuerier
	Embedder                search.Embedder
	EmbedThreshold          float64
	AmbientMaxChars         int
	MCPMgr                  MCPManager
	skillEmbedCacheMu       sync.RWMutex
	skillEmbedCache         map[string][]float32
}

func NewPromptAssemblyService(
	promptMgr protocol.PromptFacade,
	soulMDContent *string,
	personaRefiner *agentctx.PersonaRefiner,
	baseSystemPromptTpl string,
	registry protocol.LLMRegistry,
	serverPlatform string,
	toolReg protocol.ToolRegistry,
	db protocol.SQLQuerier,
	embedder search.Embedder,
	embedThreshold float64,
	ambientMaxChars int,
	mcpMgr MCPManager,
	activatedSystemPrompt string,
) *PromptAssemblyService {
	return &PromptAssemblyService{
		PromptMgr:             promptMgr,
		SoulMDContent:         soulMDContent,
		PersonaRefiner:        personaRefiner,
		BaseSystemPromptTpl:   baseSystemPromptTpl,
		Registry:              registry,
		ServerPlatform:        serverPlatform,
		ToolReg:               toolReg,
		DB:                    db,
		Embedder:              embedder,
		EmbedThreshold:        embedThreshold,
		AmbientMaxChars:       ambientMaxChars,
		MCPMgr:                mcpMgr,
		ActivatedSystemPrompt: activatedSystemPrompt,
		skillEmbedCache:       make(map[string][]float32),
	}
}
