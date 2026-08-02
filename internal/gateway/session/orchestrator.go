package session

import (
	"context"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Orchestrator 会话编排领域服务（GD-13-008，见 04-arch-refactor.md A-03）。
// 收敛 SSE 与 Headless（Cron/Workflow/Webhook）两条入口重复实现的会话生命周期
// 编排：会话确保/历史加载/Hook 分发/斜线命令路由/上下文压缩决策与执行/
// SystemPromptGuard 装配/用户与助手消息持久化/会话标题与 TouchSession/
// Transcript 写入/FSM 驱动与事件消费。
//
// 历史代价（本次收敛动机）：SystemPromptGuard 曾只接在 SSE 路径，headless
// 路径遗漏（见原 internal/agent/pool.go headlessPromptGuard 补丁注释）——
// 各自实现导致防护漏接的物证。A-03 Step5 起，两条路径统一经本包的
// headlessPromptGuard()/newSystemPromptGuard() 收口。
//
// 2026-08-02 范围调整：原规划设想的第三条"非流式"入口
// （server_handlers.go:136）经核实从未以会话编排重复实现的形态存在，详见
// local_playground/upgrade/99-new-findings.md
// "发现于 04-arch-refactor（A-03，Step6 目标不存在）"。本次收敛范围为
// SSE + Headless 两条入口，由 Request.Headless 区分子路径。
type Orchestrator interface {
	RunTurn(ctx context.Context, req Request, sink Sink) (*Result, error)
}

// Deps 构造 Orchestrator 所需的全部窄接口依赖（HE-3：接口在调用方/本包定义，
// 不直接 import chat 包具体类型）。由 chat 包（SSE 入口）与 cmd/polaris 装配层
// （Headless 入口）分别提供满足这些接口的具体实现适配器。
type Deps struct {
	Hooks         HookRunner
	SlashRouter   SlashDispatcher
	Compression   CompressionEngine
	Persistence   Persistence
	Prompt        PromptAssembler
	Registry      protocol.LLMRegistry
	AgentPool     protocol.AgentPool
	TranscriptDir string
	DataDir       string
}

type orchestrator struct {
	hooks         HookRunner
	slash         SlashDispatcher
	compression   CompressionEngine
	persistence   Persistence
	prompt        PromptAssembler
	registry      protocol.LLMRegistry
	agentPool     protocol.AgentPool
	transcriptDir string
	dataDir       string
}

// New 构造 Orchestrator。
func New(d Deps) Orchestrator {
	return &orchestrator{
		hooks:         d.Hooks,
		slash:         d.SlashRouter,
		compression:   d.Compression,
		persistence:   d.Persistence,
		prompt:        d.Prompt,
		registry:      d.Registry,
		agentPool:     d.AgentPool,
		transcriptDir: d.TranscriptDir,
		dataDir:       d.DataDir,
	}
}

// RunTurn 驱动一轮完整会话编排，按 Request.Headless 分派到交互式
// （AgentPool.Acquire + per-session 长驻 Agent）或 Headless（AgentPool.
// AcquireHeadless + 一次性 Agent）两条子路径——池化语义不同，不可合并为一套
// Acquire 调用（A-03 Step5 设计记录）。
func (o *orchestrator) RunTurn(ctx context.Context, req Request, sink Sink) (*Result, error) {
	if req.Headless {
		return o.runHeadless(ctx, req, sink)
	}
	return o.runInteractive(ctx, req, sink)
}

// resolveSessionID 会话确保子步骤：空 SessionID 视为新会话并生成 ID；非空时
// 经 SessionIDPattern 白名单校验（S-07，双重防御第二层——HTTP 边界层的第一层
// 早期校验见 chat/sse.go）。
func (o *orchestrator) resolveSessionID(req Request) (sessionID string, isNewSession bool, err error) {
	sessionID = strings.TrimSpace(req.SessionID)
	isNewSession = sessionID == ""
	if isNewSession {
		sessionID = newSessionID()
	}
	if !SessionIDPattern.MatchString(sessionID) {
		return "", false, apperr.New(apperr.CodeInvalidInput, "session.RunTurn: invalid session_id")
	}
	return sessionID, isNewSession, nil
}

// emitError 统一的错误事件出口：按 code 分级日志（部分 code 视为预期内的
// 降级路径，用 Warn 而非 Error，行为与原 chat.WriteSSEError 完全等价）+
// 经 sink 推送 KindError 事件。
func (o *orchestrator) emitError(sink Sink, code, message, sessionID string, err error) {
	if code == "hook_blocked" || code == "empty_response" || code == "no_provider" {
		slog.Warn("session: turn error", "code", code, "session", sessionID, "message", message, "err", err)
	} else {
		slog.Error("session: turn error", "code", code, "session", sessionID, "message", message, "err", err)
	}
	_ = sink.Emit(Event{
		Kind:    KindError,
		Payload: map[string]any{"code": code, "message": message},
		Err:     err,
	})
}
