package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/llm/tts"
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
	TTSEngine *atomic.Pointer[tts.ProviderBox]
	WriteSSE  func(http.ResponseWriter, http.Flusher, string, any)

	// Embedder 语义向量化引擎（nil = Tier 1 词元重叠降级）。
	// 由 boot_server.go 通过 SetEmbedder 注入；聊天主流程不依赖此字段，可安全为 nil。
	Embedder search.Embedder

	// EmbedThreshold Tier 2 余弦相似度阈值（默认 0.60，由 cfg.Embedding.Threshold 注入）。
	EmbedThreshold float64
}

func NewChatHandler(
	db protocol.SQLQuerier,
	chatRepo protocol.ChatRepository,
	providerRepo protocol.ProviderRepository,
	systemRepo repo.SystemRepository,
	agentPool AgentPool,
	bb protocol.Blackboard,
	compressor *Compressor,
	transcriptDir string,
	sttEngine *atomic.Pointer[STTEngineBox],
	ttsEngine *atomic.Pointer[tts.ProviderBox],
	writeSSE func(http.ResponseWriter, http.Flusher, string, any),
) *ChatHandler {
	return &ChatHandler{
		DB:            db,
		ChatRepo:      chatRepo,
		ProviderRepo:  providerRepo,
		SystemRepo:    systemRepo,
		AgentPool:     agentPool,
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

func (h *ChatHandler) GenerateReply(ctx context.Context, req *apptypes.InferRequest, sessionID string) (string, error) { //nolint:gocyclo
	history, err := h.LoadMessages(ctx, sessionID)
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
