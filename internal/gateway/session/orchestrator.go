package session

import (
	"context"
	"errors"
	"fmt"
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
	WorkspaceDir  string
	// Turns 回合步骤与元数据持久化（ADR-0118）；nil 时步骤只实时推送、不落库，消息也不带用量摘要。
	Turns TurnStore
	// Goals 跨回合目标存储（ADR-0120 决策七）；nil 时不做目标续跑。
	Goals GoalStore
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
	workspaceDir  string
	turns         TurnStore
	goals         GoalStore
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
		workspaceDir:  d.WorkspaceDir,
		turns:         d.Turns,
		goals:         d.Goals,
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

// emitError 统一错误出口：日志只记安全元数据，事件携带固定公开摘要和无 Cause 的分类错误。
func (o *orchestrator) emitError(ctx context.Context, sink Sink, kind string, fallback apperr.Code, sessionID string, err error) {
	code := fallback
	if err != nil {
		code = publicErrorCodeFor(err)
	}
	code = publicErrorCode(string(code))
	message := publicTurnErrorMessage(kind)
	level := slog.LevelError
	if kind == "hook_blocked" || kind == "empty_response" || kind == "no_provider" {
		level = slog.LevelWarn
	}
	slog.LogAttrs(ctx, level, "session: turn error",
		slog.String("operation", kind),
		slog.String("code", string(code)),
		slog.String("session", sessionID),
		slog.String("error_type", safeErrorType(err)))
	_ = sink.Emit(Event{
		Kind: KindError,
		Payload: map[string]any{
			"code":    string(code),
			"kind":    kind,
			"message": message,
		},
		Err: apperr.New(code, message),
	})
}

func publicTurnErrorMessage(kind string) string {
	switch kind {
	case "hook_blocked":
		return "请求被安全策略拦截"
	case "no_provider":
		return "当前没有可用模型，请检查模型配置"
	case "no_agent":
		return "会话服务暂不可用，请稍后重试"
	case "session_error":
		return "会话初始化失败，请稍后重试"
	case "history_error":
		return "会话历史读取失败，请稍后重试"
	case "agent_pool_error":
		return "会话 Agent 暂不可用，请稍后重试"
	case "empty_response", "fsm_error":
		return "模型调用失败，请稍后重试"
	default:
		return "请求处理失败，请稍后重试"
	}
}

func publicErrorCode(raw string) apperr.Code {
	return apperr.NormalizeErrorCode(apperr.Code(raw))
}

func publicErrorCodeFor(err error) apperr.Code {
	if errors.Is(err, context.Canceled) {
		return apperr.CodeCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apperr.CodeTimeout
	}
	return publicErrorCode(string(apperr.CodeOf(err)))
}

func safeErrorType(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

func safeErrorSummary(err error) string {
	return fmt.Sprintf("code=%s type=%s", publicErrorCodeFor(err), safeErrorType(err))
}
