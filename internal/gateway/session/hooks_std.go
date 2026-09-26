package session

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/action/hook"
	"github.com/polarisagi/polaris/internal/protocol"
)

// StandardHooks 以 hooks.json 引擎实现会话与压缩的事件接口。引擎在网关构造之后注入
// （SetRunner），未注入时全部事件为空操作。
type StandardHooks struct {
	runner atomic.Pointer[hook.Runner]
}

func NewStandardHooks() *StandardHooks { return &StandardHooks{} }

// SetRunner 注入 hooks.json 引擎。
func (s *StandardHooks) SetRunner(r *hook.Runner) { s.runner.Store(r) }

func (s *StandardHooks) dispatch(ctx context.Context, sessionID string, in hook.Input) HookVerdict {
	r := s.runner.Load()
	if r == nil {
		return HookVerdict{}
	}
	in.SessionID = sessionID
	ctx = context.WithValue(ctx, protocol.CtxSessionIDKey{}, sessionID)
	out := r.Dispatch(ctx, in)
	return HookVerdict{Blocked: out.Block, Reason: out.Reason, AdditionalContext: strings.Join(out.AdditionalContext, "\n"),
		StopTurn: out.Stop, StopReason: out.StopReason}
}

func (s *StandardHooks) SessionStart(ctx context.Context, sessionID, source string) HookVerdict {
	return s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventSessionStart, Source: source})
}

func (s *StandardHooks) UserPromptSubmit(ctx context.Context, sessionID, prompt string) HookVerdict {
	return s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventUserPromptSubmit, Prompt: prompt})
}

func (s *StandardHooks) Stop(ctx context.Context, sessionID, lastReply string, stopHookActive bool) HookVerdict {
	return s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventStop, StopHookActive: stopHookActive,
		Message: lastReply})
}

func (s *StandardHooks) StopFailure(ctx context.Context, sessionID, reason string) {
	s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventStopFailure, Reason: reason})
}

func (s *StandardHooks) SessionEnd(ctx context.Context, sessionID, reason string) {
	s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventSessionEnd, Reason: reason})
}

// PreCompact continue:false 或阻断时跳过压缩（Codex 语义）。
func (s *StandardHooks) PreCompact(ctx context.Context, sessionID, trigger string) HookVerdict {
	return s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventPreCompact, Trigger: trigger})
}

func (s *StandardHooks) PostCompact(ctx context.Context, sessionID, trigger string) {
	s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventPostCompact, Trigger: trigger})
}

// Interrupt 用户中断回合（Codex Interrupt 事件；仅通知，无决策输出）。
func (s *StandardHooks) Interrupt(ctx context.Context, sessionID, reason string) {
	s.dispatch(ctx, sessionID, hook.Input{HookEventName: hook.EventInterrupt, Reason: reason})
}
