package session

import (
	"context"

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
// 路径遗漏（见 internal/agent/pool.go 原 headlessPromptGuard 补丁注释）——
// 各自实现导致防护漏接的物证。
//
// 2026-08-02 范围调整：原规划设想的第三条"非流式"入口
// （server_handlers.go:136）经核实从未以会话编排重复实现的形态存在（自始至
// 终是异步 Blackboard 任务投递），详见
// local_playground/upgrade/99-new-findings.md
// "发现于 04-arch-refactor（A-03，Step6 目标不存在）"。本次收敛范围为
// SSE + Headless 两条入口。
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

// RunTurn 驱动一轮完整会话编排。
//
// [A-03 Step1] 本 commit 仅搭建包骨架与依赖注入契约，不改动任何既有文件，
// RunTurn 尚无消费方，函数体为占位实现。具体编排逻辑（现存于
// internal/gateway/server/chat/sse.go HandleAgentStream/handleAgentStreamFSM）
// 逐段搬入见 A-03 Step2 commit。
func (o *orchestrator) RunTurn(ctx context.Context, req Request, sink Sink) (*Result, error) {
	return nil, apperr.New(apperr.CodeInternal, "session.Orchestrator.RunTurn: not yet implemented (A-03 Step2)")
}
