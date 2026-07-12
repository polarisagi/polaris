package chat

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/store/search"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

type SessionCompressor interface {
	Stats(msgs []apptypes.Message) types.ContextStats
	ForceCompact(ctx context.Context, sessionID string, msgs []apptypes.Message, provider protocol.Provider, mem MemoryFacade) ([]apptypes.Message, types.CompactResult, error)
}

type ChatHandler struct {
	DB            protocol.SQLQuerier
	ChatRepo      protocol.ChatRepository
	ProviderRepo  protocol.ProviderRepository
	SystemRepo    repo.SystemRepository
	AgentPool     protocol.AgentPool
	Blackboard    protocol.Blackboard
	Compressor    *Compressor
	SlashRouter   *SlashCommandRouter
	TranscriptDir string
	PromptMgr     protocol.PromptFacade
	SoulMDContent *string

	Hooks                   HookRunner
	DataDir                 string
	Registry                protocol.LLMRegistry
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

	// OutboxWriter 供 SaveMessage 在直写 chat_messages 重试耗尽后做异步兜底
	// 投递（GD-13-004 复核修复，见 chat_message_persist_handler.go）。nil 时
	// SaveMessage 降级为仅记录错误日志（与修复前行为一致）。
	OutboxWriter protocol.OutboxWriter
}

type Dependencies struct {
	DB                    protocol.SQLQuerier
	ChatRepo              protocol.ChatRepository
	ProviderRepo          protocol.ProviderRepository
	SystemRepo            repo.SystemRepository
	AgentPool             protocol.AgentPool
	Blackboard            protocol.Blackboard
	Compressor            *Compressor
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
	EnableFSMChatPath     bool
	OutboxWriter          protocol.OutboxWriter
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
		OutboxWriter:          deps.OutboxWriter,
		skillEmbedCache:       make(map[string][]float32),
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
