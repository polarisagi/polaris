package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// AgentPool 管理 per-session Agent 生命周期。
// Acquire 返回该 session 专属 Agent 及 release 回调；调用方 defer release()。
// 超出容量时 Acquire 阻塞最多 100ms，超时返回 apperr.CodeResourceExhausted。
type AgentPool interface {
	Acquire(ctx context.Context, sessionID string) (protocol.AgentController, func(), error)
}

type SessionCompressor interface {
	Stats(msgs []apptypes.Message) types.ContextStats
	ForceCompact(ctx context.Context, sessionID string, msgs []apptypes.Message, provider protocol.Provider, mem MemoryFacade) ([]apptypes.Message, types.CompactResult, error)
}

type ChatHandler struct {
	DB            protocol.SQLQuerier
	ChatRepo      protocol.ChatRepository
	ProviderRepo  protocol.ProviderRepository
	SystemRepo    repo.SystemRepository
	AgentPool     AgentPool
	Blackboard    protocol.Blackboard
	Compressor    *Compressor
	SlashRouter   *SlashCommandRouter
	TranscriptDir string
	PromptMgr     PromptManager
	SoulMDContent *string
	ToolStage     interface {
		SelectFor(ctx context.Context, query string) []apptypes.ToolSchema
	}
	ToolProvider interface {
		ExecuteTool(ctx context.Context, name string, args []byte) (*apptypes.ToolResult, error)
	}

	Hooks                   HookRunner
	DataDir                 string
	Registry                LLMRegistry
	SkillReg                protocol.SkillRegistry
	ToolReg                 protocol.ToolRegistry
	MCPMgr                  MCPManager
	ServerPlatform          string
	BaseSystemPromptTpl     string
	ActivatedSystemPromptMu sync.RWMutex
	ActivatedSystemPrompt   string
	LogStore                interface {
		Append(entry any)
		Subscribe() chan any
		Unsubscribe(chan any)
	}

	STTEngine *atomic.Pointer[STTEngineBox]
	TTSEngine *atomic.Pointer[TTSProviderBox]
	WriteSSE  func(http.ResponseWriter, http.Flusher, string, any)

	// Embedder 语义向量化引擎（nil = Tier 1 词元重叠降级）。
	// 由 boot_server.go 通过 SetEmbedder 注入；聊天主流程不依赖此字段，可安全为 nil。
	Embedder search.Embedder

	// EmbedThreshold Tier 2 余弦相似度阈值（默认 0.60，由 cfg.Embedding.Threshold 注入）。
	EmbedThreshold float64

	// skillEmbedCacheMu/skillEmbedCache 技能文本→向量缓存（sha256(text) 为 key）。
	// 依赖注入替代包级可变变量（R1.3）：原实现是 sse.go 里的包级 var，
	// 2026-07-07 复核发现后收敛为按 ChatHandler 实例持有——每个 ChatHandler
	// 生命周期独立，测试互不污染，且与本文件其它并发状态管理方式一致。
	skillEmbedCacheMu sync.RWMutex
	skillEmbedCache   map[string][]float32

	// ContextRefExpander 展开用户消息中的 @file/@url/git 引用（nil = 跳过展开，
	// 兼容未注入场景）。2026-07-08 修复：此前 authcontext.ContextRefExpander
	// 全仓零调用方，功能完整且有独立测试覆盖但从未接线，现接入 HandleAgentStream
	// 消息预处理入口（见 sse.go）。
	ContextRefExpander *authcontext.ContextRefExpander
	EnableFSMChatPath  bool
}

type Dependencies struct {
	DB                    protocol.SQLQuerier
	ChatRepo              protocol.ChatRepository
	ProviderRepo          protocol.ProviderRepository
	SystemRepo            repo.SystemRepository
	AgentPool             AgentPool
	Blackboard            protocol.Blackboard
	Compressor            *Compressor
	TranscriptDir         string
	PromptMgr             PromptManager
	SoulMDContent         *string
	Hooks                 HookRunner
	DataDir               string
	Registry              LLMRegistry
	ServerPlatform        string
	BaseSystemPromptTpl   string
	ActivatedSystemPrompt string
	STTEngine             *atomic.Pointer[STTEngineBox]
	TTSEngine             *atomic.Pointer[TTSProviderBox]
	WriteSSE              func(http.ResponseWriter, http.Flusher, string, any)
	ContextRefExpander    *authcontext.ContextRefExpander
	EnableFSMChatPath     bool
}

// NewChatHandler 故意不做构造函数级 fail-closed nil 强制校验（2026-07-08 复核
// code-quality-remediation-verification-20260707.md Phase 4 遗留项后的结论，
// 详见 local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md）：
//  1. 全部 HTTP 路由已由 withMiddleware 的 PanicRecovery 兜底
//     （server_lifecycle.go:190 → middleware_auth.go "[P0修复] panic recovery"），
//     单个 handler 因硬依赖为 nil panic 只返回 500 并记录堆栈，不影响进程存活；
//  2. sysadmin 包已有先例以部分 Dependencies 构造 handler（见
//     sysadmin/handler_wiring_test.go 仅传 DB 字段的回归测试），构造函数层面
//     强制要求"全部字段非 nil"会破坏这一既有测试模式；
//  3. 真正会导致整个进程崩溃（而非单请求 500）的风险点是脱离 HTTP 中间件、
//     没有自身 recover 的后台 goroutine（cronadmin 的 cron/event 调度 +
//     executeAutomation 正是此类），已改用 pkg/concurrent.SafeGo 修复，
//     而不是在此处加构造函数校验；
//  4. 唯一发现的真实解引用风险（SoulMDContent *string）已在 system_prompt.go
//     补 nil-safe 判空，成本极低且不影响任何调用方。
func NewChatHandler(deps Dependencies) *ChatHandler {
	return &ChatHandler{
		DB:                    deps.DB,
		ChatRepo:              deps.ChatRepo,
		ProviderRepo:          deps.ProviderRepo,
		SystemRepo:            deps.SystemRepo,
		AgentPool:             deps.AgentPool,
		Blackboard:            deps.Blackboard,
		Compressor:            deps.Compressor,
		SlashRouter:           NewSlashCommandRouter(deps.Compressor, deps.ChatRepo, deps.WriteSSE),
		TranscriptDir:         deps.TranscriptDir,
		PromptMgr:             deps.PromptMgr,
		SoulMDContent:         deps.SoulMDContent,
		Hooks:                 deps.Hooks,
		DataDir:               deps.DataDir,
		Registry:              deps.Registry,
		ServerPlatform:        deps.ServerPlatform,
		BaseSystemPromptTpl:   deps.BaseSystemPromptTpl,
		ActivatedSystemPrompt: deps.ActivatedSystemPrompt,
		STTEngine:             deps.STTEngine,
		TTSEngine:             deps.TTSEngine,
		WriteSSE:              deps.WriteSSE,
		ContextRefExpander:    deps.ContextRefExpander,
		EnableFSMChatPath:     deps.EnableFSMChatPath,
		skillEmbedCache:       make(map[string][]float32),
	}
}

type HookRunner interface {
	Fire(event string, env map[string]string)
	FireBefore(event string, env map[string]string) (blocked bool, reason string)
}

func (h *ChatHandler) GenerateReply(ctx context.Context, req *apptypes.InferRequest, sessionID string) (string, error) { //nolint:gocyclo
	history, err := h.ListMessages(ctx, sessionID)
	if err != nil {
		return "", err
	}

	p := h.Registry.PickProvider("default")
	if p == nil {
		p = h.Registry.PickProvider("general")
	}
	if p == nil {
		return "", apperr.New(apperr.CodeInternal, "no provider available")
	}

	var toolSchemas []apptypes.ToolSchema
	if h.ToolStage != nil {
		toolSchemas = h.ToolStage.SelectFor(ctx, "")
	}

	var sb strings.Builder
	const maxToolRounds = 10
	for range maxToolRounds {
		//nolint:bare-infer // 历史代码暂留，后续重构替换
		ch, err := p.StreamInfer(ctx, history,
			apptypes.WithMaxTokens(2048),
			apptypes.WithTemperature(0.7),
			apptypes.WithTools(toolSchemas),
		)
		if err != nil {
			return "", err
		}

		var roundText strings.Builder
		var toolCalls []map[string]json.RawMessage
		for ev := range ch {
			switch ev.Type {
			case apptypes.StreamTextDelta:
				if ev.Content != "" {
					roundText.WriteString(ev.Content)
					sb.WriteString(ev.Content)
				}
			case apptypes.StreamToolCall:
				var call map[string]json.RawMessage
				if json.Unmarshal([]byte(ev.Content), &call) == nil {
					toolCalls = append(toolCalls, call)
				}
			}
		}

		if len(toolCalls) == 0 || h.ToolProvider == nil {
			break
		}

		assistantParts := make([]any, 0, 1+len(toolCalls))
		if roundText.Len() > 0 {
			assistantParts = append(assistantParts, map[string]any{"type": "text", "text": roundText.String()})
		}
		toolResultParts := make([]any, 0, len(toolCalls))
		for _, tc := range toolCalls {
			var toolID, toolName string
			var inputRaw json.RawMessage
			if b, ok := tc["id"]; ok {
				json.Unmarshal(b, &toolID) //nolint:errcheck
			}
			if b, ok := tc["name"]; ok {
				json.Unmarshal(b, &toolName) //nolint:errcheck
			}
			if b, ok := tc["input"]; ok {
				inputRaw = b
			}
			assistantParts = append(assistantParts, map[string]any{
				"type": "tool_use", "id": toolID, "name": toolName, "input": inputRaw,
			})

			result, execErr := h.ToolProvider.ExecuteTool(ctx, toolName, inputRaw)
			var resultText string
			if execErr != nil {
				resultText = "error: " + execErr.Error()
			} else if result != nil {
				resultText = string(result.Output)
				if result.Error != "" {
					if resultText != "" {
						resultText += "\n"
					}
					resultText += "error: " + result.Error
				}
			}
			toolResultParts = append(toolResultParts, map[string]any{
				"type": "tool_result", "tool_use_id": toolID, "content": resultText,
			})
		}
		history = append(history,
			apptypes.Message{Role: "assistant", Parts: assistantParts},
			apptypes.Message{Role: "user", Parts: toolResultParts},
		)
	}

	return sb.String(), nil
}

func (h *ChatHandler) RunPostProcessors(ctx context.Context, sessionID, reply string) {
	if reply == "" {
		return
	}
	_ = h.SaveMessage(ctx, sessionID, "assistant", reply, "", "", 0)
	_ = h.UpdateSessionTitle(ctx, sessionID, reply)
	_ = h.TouchSession(ctx, sessionID) // error logged inside
}
