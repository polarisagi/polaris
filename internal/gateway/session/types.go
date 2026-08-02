// Package session 收敛 SSE 与 Headless（Cron/Workflow/Webhook）两条入口重复
// 实现的会话生命周期编排（GD-13-008，04-arch-refactor.md A-03）。
//
// 零 net/http 依赖——由 internal/lint Test_inv_M13_SessionPkgNoHTTP 强制门控。
// 依赖以本文件声明的窄接口注入（HE-3：接口在调用方定义），不直接 import
// internal/gateway/server/chat 的具体类型，避免 chat→session→chat 循环依赖
// （chat/sse_sink.go 反向实现 session.Sink，见 A-03 Step3）。
//
// 范围调整记录：原 04-arch-refactor.md 设想的第三条"非流式"入口
// （server_handlers.go:136）经核实从未以会话编排重复实现的形态存在（自始至
// 终是异步 Blackboard 任务投递，见 local_playground/upgrade/99-new-findings.md
// "发现于 04-arch-refactor（A-03，Step6 目标不存在）"），本包收敛范围为
// SSE + Headless 两条入口。
package session

import (
	"context"

	gwtypes "github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── 领域事件（Sink 的传输载荷）──────────────────────────────────────────────

// EventKind 领域事件类型。HTTP/SSE 层（chat/sse_sink.go）负责把 Kind 翻译成
// 现有 SSE wire 事件名（"token"/"status"/"context_warning"/"error"/"complete"），
// Orchestrator 本身不感知 SSE 帧格式（HE-3：产出方与消费方解耦）。
type EventKind int

const (
	KindDelta EventKind = iota
	// KindReasoning 对应 wire 事件 "reasoning"（LLM 思考过程占位文本），Text 携带内容，
	// 不计入 BufferSink 累积的最终回复（与 KindDelta 的关键区别）。
	KindReasoning
	// KindStatus 对应 wire 事件 "status"，Payload 直接透传（含 type/message 等既有字段）。
	KindStatus
	KindContextWarning
	KindToolCall
	KindComplete
	KindError
	// KindSystemNotice 对应 wire 事件 "system_notice"（Agent 池资源降级提示，
	// 独立事件名，非嵌套在 KindStatus 的 "status" 事件内），Payload 直接透传。
	KindSystemNotice
)

// Event 领域事件。Text 用于 KindDelta 增量文本；Payload 用于 KindStatus /
// KindContextWarning / KindComplete 的结构化载荷；Err 仅 KindError 携带，
// 供 Sink 侧映射 HTTP 状态码/错误 code。
type Event struct {
	Kind    EventKind
	Text    string
	Payload map[string]any
	Err     error
}

// Sink 领域事件出口。HTTP/SSE、非流式（bufferSink）、Headless 各实现一个。
// Emit 返回 error 表示下游已不可写（客户端断连等），Orchestrator 应转入
// abort 收尾路径（保存已产出内容 + Interrupt Agent）。
type Sink interface {
	Emit(ev Event) error
}

// ── 请求/结果 ────────────────────────────────────────────────────────────

// Attachment 由 chat 包内私有的 sseAttachment 提升而来（A-03 Step2 迁移时
// chat 侧改为类型别名，不留双轨）。
type Attachment struct {
	URI      string
	MimeType string
	Name     string
	Data     string // legacy base64（向后兼容旧版协议）
}

// Request 一轮对话的输入。
//
// ImageParts 取 []types.ImagePart（已解码字节，非 base64 字符串）：HTTP 边界
// 层（chat/sse.go 瘦身后的 HandleAgentStream）负责把 wire 协议的 base64
// sseImagePart 解码为 types.ImagePart 再构造 Request——这是纯粹的协议格式转译，
// 不含业务决策，留在 HTTP 层不违反"编排逻辑归 Orchestrator"的边界。而
// Attachments（VFS workspace:// 引用，含磁盘 IO/视频大小门控/图片与视频分派）
// 是真正的领域逻辑（buildStreamUserMessage 原实现），随 A-03 Step2 迁入
// RunTurn 内部处理，Request 只携带未解析的引用。
type Request struct {
	SessionID   string
	Input       string
	ModelID     string
	Attachments []Attachment
	ImageParts  []types.ImagePart
	// Channel 取值 "web" | "cli" | "cron" | "webhook" | "workflow"，进入 Hook
	// 环境变量 POLARIS_CHANNEL。
	Channel string
	// Streaming false 时 Orchestrator 内部缓冲（配 BufferSink），Result.Reply
	// 携带完整回复；true 时逐 token 经 sink.Emit(KindDelta) 增量推送。
	Streaming bool
	// Headless=true 时走 AgentPool.AcquireHeadless 一次性子路径（Cron/
	// Workflow/Webhook），=false 时走 AgentPool.Acquire 交互式子路径（SSE，
	// per-session 长驻 Agent 实例）。二者池化语义不同（A-03 Step5 设计），
	// 保留显式区分而非仅凭 Channel 字符串推断。
	Headless bool
	// WorkingDir 仅 Headless=true 时可能非空（Workflow/Cron 任务的工作目录，
	// 拼入 types.Intent.WorkingDir）。
	WorkingDir string
	// TitleHint 会话标题来源覆盖；为空时 RunTurn 用 Input 作为标题来源。
	// 保留字段是为了不改变 Headless 三个既有调用方（workflow_engine.go /
	// cron_runner.go）此前传自动化任务名（而非用户输入原文）作为标题的行为。
	TitleHint string
	// RunID / ReasoningEffort 原 agentStreamRequest 字段，仅交互式路径在
	// AgentPool 资源耗尽降级判定时使用（区分后台提炼请求 vs 前台对话请求）。
	RunID           string
	ReasoningEffort string
	// Metadata 额外注入 message.before/message.after/turn.stop 等 Hook 环境变量
	// 的调用方专属字段（如 Webhook 的 POLARIS_USER_ID/POLARIS_CHAT_ID），随
	// SessionID/Channel 等通用字段一起合入 Fire/FireBefore 的 env map（通用键
	// 优先，Metadata 不能覆盖 POLARIS_MESSAGE/POLARIS_SESSION_ID/
	// POLARIS_CHANNEL/POLARIS_REPLY）。Cron/Workflow 无对应概念时留空即可，
	// 三条 Headless 调用方此前 message.after/turn.stop hook 覆盖不一致（仅
	// Webhook 分支接了），A-03 Step5 起统一由 runHeadless 触发（见
	// orchestrator_headless.go）。
	Metadata map[string]string
}

// Result 一轮对话的结果。
type Result struct {
	SessionID    string
	Reply        string
	LatencyMs    int64
	Aborted      bool
	SlashHandled bool
}

// CommandResult 斜线命令执行结果（SlashDispatcher.Dispatch 返回）。
// 与 chat.CommandResult 字段等价，本包独立声明以避免 session 包反向 import
// chat 包（HE-3 + 避免循环依赖）。
type CommandResult struct {
	// Handled=true 表示命令已处理，调用方应短路（跳过 LLM 推理）。
	Handled bool
	// Response 是助手回复文本，非空时由调用方持久化到 DB。
	Response string
	// UpdatedHistory 是命令执行后的消息历史（/compact 和 /clear 会修改）。
	UpdatedHistory []types.Message
}

// ── 消费端窄接口（HE-3：接口在调用方定义，不直接 import chat 包具体类型）──

// MemoryFacade 会话编排对记忆门面的消费端接口（仅 Stage 3 渲染 Task Canvas
// 时调用）。与 chat.MemoryFacade 方法集完全一致，Go 结构化类型无需显式转换。
type MemoryFacade interface {
	RenderTaskCanvas() string
}

// HookRunner 会话编排对 Hook 系统的消费端接口。
type HookRunner interface {
	Fire(event string, env map[string]string)
	FireBefore(event string, env map[string]string) (blocked bool, reason string)
}

// Persistence 会话编排对消息/会话持久化的消费端接口。
// 实现：chat.ChatPersistenceService（经 chat 包内适配器注入，方法集已完全匹配，
// 无需额外包装）。
type Persistence interface {
	EnsureSession(ctx context.Context, sessionID string) error
	ListMessages(ctx context.Context, sessionID string) ([]types.Message, error)
	SaveMessage(ctx context.Context, sessionID, role, content, toolCalls, reasoningContent string, durationMs int64) error
	UpdateSessionTitle(ctx context.Context, sessionID, firstInput string) error
	TouchSession(ctx context.Context, sessionID string) error
	SampleAndScoreReply(sessionID, query, response string)
}

// PromptAssembler 会话编排对系统提示词组装的消费端接口。
// ActivatedSystemPrompt/ExpandContextRefs 是 A-03 Step2 新增的薄适配方法
// （包装既有字段访问，不改变行为），供 chat.PromptAssemblyService 满足本接口。
type PromptAssembler interface {
	InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []types.Message, userQuery string) []types.Message
	// ReadActivatedSystemPrompt 返回当前激活的系统提示词（M9 GEPA 动态激活
	// 提示词，可能为空）。包装 chat.PromptAssemblyService.ActivatedSystemPromptMu
	// 读锁访问，避免 session 包直接持有 sync.RWMutex 字段。命名避开
	// chat.PromptAssemblyService 已有的同名导出字段 ActivatedSystemPrompt
	// （Go 不允许方法与字段同名）。
	ReadActivatedSystemPrompt() string
	// ExpandContextRefs 展开 @file/@url/git: 引用；ContextRefExpander 未注入时
	// 原样返回 input；单条引用展开失败计入 skipped 但不阻断。
	ExpandContextRefs(ctx context.Context, input string) (expanded string, skipped []string)
}

// CompressionEngine 会话编排对上下文压缩的消费端接口。
type CompressionEngine interface {
	Stats(msgs []types.Message) gwtypes.ContextStats
	WarnPct() float64
	NeedsCompact(msgs []types.Message) bool
	Compact(ctx context.Context, sessionID string, msgs []types.Message, provider protocol.Provider, mem MemoryFacade) ([]types.Message, gwtypes.CompactResult, error)
	ForceCompact(ctx context.Context, sessionID string, msgs []types.Message, provider protocol.Provider, mem MemoryFacade) ([]types.Message, gwtypes.CompactResult, error)
}

// SlashDispatcher 会话编排对斜线命令路由的消费端接口。
// 与 chat.SlashCommandRouter.Dispatch 的既有签名相比，用 Sink 取代
// (http.ResponseWriter, http.Flusher)——这是 session 包"零 net/http 依赖"
// 硬约束下的必要解耦，随 A-03 Step2 一并修改 slash_commands.go 签名。
type SlashDispatcher interface {
	Dispatch(ctx context.Context, input, sessionID string, history []types.Message, provider protocol.Provider, sink Sink, mem MemoryFacade) CommandResult
}
