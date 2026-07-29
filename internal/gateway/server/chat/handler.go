package chat

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/eval/analysis"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/security/taint"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

type SessionCompressor interface {
	Stats(msgs []apptypes.Message) types.ContextStats
	ForceCompact(ctx context.Context, sessionID string, msgs []apptypes.Message, provider protocol.Provider, mem MemoryFacade) ([]apptypes.Message, types.CompactResult, error)
}

type ChatHandler struct {
	AgentPool      protocol.AgentPool
	Blackboard     protocol.Blackboard
	ChannelRepo    repo.ChannelRepository
	ProviderRepo   protocol.ProviderRepository
	SystemRepo     repo.SystemRepository
	Registry       protocol.LLMRegistry
	ToolReg        protocol.ToolRegistry
	SkillReg       protocol.SkillRegistry
	MCPMgr         MCPManager
	ServerPlatform string
	DataDir        string
	TranscriptDir  string
	Hooks          HookRunner
	SlashRouter    *SlashCommandRouter
	WriteSSE       func(http.ResponseWriter, http.Flusher, string, any)
	LogStore       interface {
		Append(entry any)
		Subscribe() chan any
		Unsubscribe(chan any)
	}
	SamplingMonitor *analysis.ContinuousSamplingMonitor

	PersistenceService *ChatPersistenceService
	PromptService      *PromptAssemblyService
	AudioService       *AudioService
	CompressionService *CompressionService
}

type Dependencies struct {
	DB                    protocol.SQLQuerier
	ChatRepo              protocol.ChatRepository
	ChannelRepo           repo.ChannelRepository
	ProviderRepo          protocol.ProviderRepository
	SystemRepo            repo.SystemRepository
	AgentPool             protocol.AgentPool
	Blackboard            protocol.Blackboard
	CompressionService    *CompressionService
	TranscriptDir         string
	PromptMgr             protocol.PromptFacade
	SoulMDContent         *string
	Hooks                 HookRunner
	DataDir               string
	Registry              protocol.LLMRegistry
	ServerPlatform        string
	BaseSystemPromptTpl   string
	ActivatedSystemPrompt string
	STTEngine             *atomic.Pointer[STTEngineBox]
	TTSEngine             *atomic.Pointer[TTSProviderBox]
	WriteSSE              func(http.ResponseWriter, http.Flusher, string, any)
	ContextRefExpander    *authcontext.ContextRefExpander
	OutboxWriter          protocol.OutboxWriter
	TaintTracker          *taint.TaintTracker
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
	persistence := NewChatPersistenceService(
		deps.ChatRepo,
		deps.DB,
		deps.OutboxWriter,
		deps.TaintTracker,
		nil,
		deps.Registry,
	)

	audio := NewAudioService(deps.STTEngine, deps.TTSEngine)

	prompt := NewPromptAssemblyService(
		deps.PromptMgr,
		deps.SoulMDContent,
		nil,
		deps.BaseSystemPromptTpl,
		deps.Registry,
		deps.ServerPlatform,
		nil,
		deps.DB,
		nil,
		0.0,
		0,
		nil,
		deps.ActivatedSystemPrompt,
	)
	prompt.ContextRefExpander = deps.ContextRefExpander

	return &ChatHandler{
		AgentPool:          deps.AgentPool,
		Blackboard:         deps.Blackboard,
		ChannelRepo:        deps.ChannelRepo,
		ProviderRepo:       deps.ProviderRepo,
		SystemRepo:         deps.SystemRepo,
		Registry:           deps.Registry,
		ServerPlatform:     deps.ServerPlatform,
		DataDir:            deps.DataDir,
		TranscriptDir:      deps.TranscriptDir,
		Hooks:              deps.Hooks,
		WriteSSE:           deps.WriteSSE,
		SlashRouter:        NewSlashCommandRouter(deps.CompressionService, deps.ChatRepo, deps.WriteSSE),
		PersistenceService: persistence,
		AudioService:       audio,
		CompressionService: deps.CompressionService,
		PromptService:      prompt,
	}
}

type HookRunner interface {
	Fire(event string, env map[string]string)
	FireBefore(event string, env map[string]string) (blocked bool, reason string)
}

// GenerateReply / RunPostProcessors 已删除（2026-07-12，Batch 9 B-5/G-2 修复）：
// 绕过 AgentController/FSM 在网关层直接开 for-loop 做 LLM 推理 + 工具执行，
// 破坏 HE-5（状态机持控制流）与 R1.9（禁止 LLM 自由流转）；全仓库零引用的死
// 代码，唯一风险是被误调用。真实聊天流程见 sse.go handleAgentStreamFSM，
// 经 AgentController.SendIntent 由 FSM 驱动。ChatDispatcher 接口同步移除
// 对应方法声明（sysadmin/handler.go）。
//
// ChatHandler.ToolStage / ToolProvider 字段级联移除（2026-07-12 复核）：二者是
// GenerateReply/RunPostProcessors 遗留的注入点（分别用于工具语义筛选与直接工具
// 执行），随宿主函数一并删除后成为纯写入无读取的孤儿字段——repo 全文 grep
// 确认 ChatHandler.ToolStage/.ToolProvider 与 Server.toolStage 在整条注入链
// （boot_server.go agentctx.NewToolStage → Server.SetToolStage → ChatHandler.
// ToolStage；server_lifecycle.go → ChatHandler.ToolProvider）上无任何读取方。
// 现整链一并移除（Server.toolStage 字段 + SetToolStage 方法 + 两处装配调用），
// 而非仅移除字段声明留下悬空 setter。agentctx.ToolStage 类型本身（语义化工具
// 筛选能力，internal/agent/context/tool_stage.go）予以保留：它是自包含的独立
// 能力单元，未来若 PRM/FSM 路径需要工具语义筛选可直接复用，不属于本次死代码
// 清理范围。

// ── SysAdmin ChatDispatcher Facade ───────────────────────────────────────────
// 这些方法实现 sysadmin.ChatDispatcher 接口，透传到内部服务。

func (h *ChatHandler) EnsureSession(ctx context.Context, sessionID string) error {
	return h.PersistenceService.EnsureSession(ctx, sessionID)
}

func (h *ChatHandler) InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []apptypes.Message, userQuery string) []apptypes.Message {
	return h.PromptService.InjectSystemPrompt(ctx, agentCtrl, history, userQuery)
}

func (h *ChatHandler) SaveMessage(ctx context.Context, sessionID, role, content, toolCalls, reasoningContent string, toolCount int64) error {
	return h.PersistenceService.SaveMessage(ctx, sessionID, role, content, toolCalls, reasoningContent, toolCount)
}

func (h *ChatHandler) UpdateSessionTitle(ctx context.Context, sessionID, firstMessage string) error {
	return h.PersistenceService.UpdateSessionTitle(ctx, sessionID, firstMessage)
}

func (h *ChatHandler) TouchSession(ctx context.Context, sessionID string) error {
	return h.PersistenceService.TouchSession(ctx, sessionID)
}

func (h *ChatHandler) ListMessages(ctx context.Context, sessionID string) ([]apptypes.Message, error) {
	return h.PersistenceService.ListMessages(ctx, sessionID)
}

func (h *ChatHandler) SampleAndScoreReply(sessionID, query, response string) {
	h.PersistenceService.SampleAndScoreReply(sessionID, query, response)
}
