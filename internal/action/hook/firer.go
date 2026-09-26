package hook

import (
	"context"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
)

// NewInput 以 context 中的会话信息构造事件输入基底。
func NewInput(ctx context.Context, ev Event) Input {
	in := Input{HookEventName: ev, PermissionMode: "default"}
	if sid, ok := ctx.Value(protocol.CtxSessionIDKey{}).(string); ok {
		in.SessionID = sid
	}
	return in
}

// FirePreToolUse 实现 sandbox.HookFirer。
func (r *Runner) FirePreToolUse(ctx context.Context, toolName string, toolInput map[string]any) sandbox.PreToolUseResult {
	in := NewInput(ctx, EventPreToolUse)
	in.ToolName, in.ToolInput = toolName, toolInput
	out := r.Dispatch(ctx, in)
	if out.Block || out.Decision == DecisionDeny {
		return sandbox.PreToolUseResult{Blocked: true, Reason: firstNonBlank(out.Reason, "denied by PreToolUse hook")}
	}
	// "ask"：Polaris 无逐次交互式权限提示通道时按拒绝处理（fail-closed），原因提示用户审批。
	if out.Decision == DecisionAsk {
		return sandbox.PreToolUseResult{Blocked: true, Reason: "PreToolUse hook requires user confirmation: " + out.Reason}
	}
	return sandbox.PreToolUseResult{UpdatedInput: out.UpdatedInput}
}

// FirePostToolUse 实现 sandbox.HookFirer。
func (r *Runner) FirePostToolUse(ctx context.Context, toolName string, toolInput map[string]any, output string, success bool, errMsg string) string {
	ev := EventPostToolUse
	if !success {
		ev = EventPostToolUseFailure
	}
	in := NewInput(ctx, ev)
	in.ToolName, in.ToolInput = toolName, toolInput
	if success {
		in.ToolResponse = output
	} else {
		in.Error = strings.TrimSpace(firstNonBlank(errMsg, output))
	}
	out := r.Dispatch(ctx, in)
	var parts []string
	if out.Block && out.Reason != "" {
		parts = append(parts, out.Reason)
	}
	parts = append(parts, out.AdditionalContext...)
	return strings.Join(parts, "\n")
}

// FirePermissionRequest 实现 hitl.PermissionHooks：deny / 阻断即拒绝；allow 不代替人工批准。
func (r *Runner) FirePermissionRequest(ctx context.Context, toolName string, toolInput map[string]any) (bool, string) {
	in := NewInput(ctx, EventPermissionRequest)
	in.ToolName, in.ToolInput = toolName, toolInput
	out := r.Dispatch(ctx, in)
	if out.Decision == DecisionDeny || out.Block {
		return true, firstNonBlank(out.Reason, "denied by PermissionRequest hook")
	}
	return false, ""
}

// FireNotification 实现 hitl.PermissionHooks（通知类事件，结果仅记录）。
func (r *Runner) FireNotification(ctx context.Context, message, notificationType string) {
	in := NewInput(ctx, EventNotification)
	in.Message, in.NotificationType = message, notificationType
	r.Dispatch(ctx, in)
}

// FireSubagentStart 子 Agent 启动（matcher 为 agent 类型）；返回注入子 Agent 上下文的附加内容。
// 不可阻断（两家语义）。
func (r *Runner) FireSubagentStart(ctx context.Context, sessionID, agentID, agentType string) string {
	in := NewInput(ctx, EventSubagentStart)
	in.SessionID = firstNonBlank(sessionID, in.SessionID)
	in.AgentID, in.AgentType = agentID, agentType
	return strings.Join(r.Dispatch(ctx, in).AdditionalContext, "\n")
}

// FireSubagentStop 子 Agent 结束；返回非空原因表示 hook 以 decision:block 要求子 Agent 继续。
func (r *Runner) FireSubagentStop(ctx context.Context, sessionID, agentID, agentType, lastMessage string, stopHookActive bool) string {
	in := NewInput(ctx, EventSubagentStop)
	in.SessionID = firstNonBlank(sessionID, in.SessionID)
	in.AgentID, in.AgentType = agentID, agentType
	in.LastAssistantMessage, in.StopHookActive = lastMessage, stopHookActive
	out := r.Dispatch(ctx, in)
	if !out.Block || out.Stop {
		return ""
	}
	return strings.TrimSpace(out.Reason)
}
