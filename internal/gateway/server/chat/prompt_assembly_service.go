package chat

import (
	"context"
	"log/slog"
	"sync"
	"time"

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
	skillEmbedCache         map[string]*skillEmbedEntry
}

// skillEmbedEntry 是技能文本→向量缓存的条目。
// storedAt 供 TTL 判定，lastUsed 供满载时的 LRU 淘汰（A-06 要求容量 + 过期双控）。
type skillEmbedEntry struct {
	vec      []float32
	storedAt time.Time
	lastUsed time.Time
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
		skillEmbedCache:       make(map[string]*skillEmbedEntry),
	}
}

// ReadActivatedSystemPrompt 满足 session.PromptAssembler 接口（A-03 Step2）。
// 包装既有 ActivatedSystemPromptMu 读锁访问，行为与 sse.go 原直接字段访问
// 完全等价，仅为满足窄接口调用形态改为方法。方法名避开同名导出字段
// ActivatedSystemPrompt（Go 不允许方法与字段重名）。
func (s *PromptAssemblyService) ReadActivatedSystemPrompt() string {
	s.ActivatedSystemPromptMu.RLock()
	defer s.ActivatedSystemPromptMu.RUnlock()
	return s.ActivatedSystemPrompt
}

// ExpandContextRefs 满足 session.PromptAssembler 接口（A-03 Step2）。
// 包装既有 ContextRefExpander nil 判空 + Expand 调用逻辑（原内联于
// sse.go HandleAgentStream 顶部），行为完全等价：ContextRefExpander 未注入时
// 原样返回 input；单条引用展开失败计入 skipped 但不阻断整轮请求。
func (s *PromptAssemblyService) ExpandContextRefs(ctx context.Context, input string) (expanded string, skipped []string) {
	if s.ContextRefExpander == nil {
		return input, nil
	}
	expandedText, report := s.ContextRefExpander.Expand(ctx, input)
	if report == nil {
		return input, nil
	}
	if len(report.Skipped) > 0 {
		slog.Warn("server: context ref expand skipped some references", "skipped", report.Skipped)
	}
	return expandedText, report.Skipped
}
