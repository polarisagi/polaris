package session

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// maxStopContinuations Stop hook 要求继续的最大续跑次数。两家以 stop_hook_active 交给 hook
// 自行防循环；宿主再加硬上限，防止写错的 hook 把一轮对话变成无限推理（HE-5：控制流在 FSM）。
const maxStopContinuations = 3

// firePromptHooks 新会话触发 SessionStart，每轮触发 UserPromptSubmit。
// 返回附加上下文与阻断原因（非空即拒绝本轮）。
func (o *orchestrator) firePromptHooks(ctx context.Context, sessionID, prompt string, isNewSession bool) (context string, blocked string) {
	var parts []string
	if isNewSession {
		v := o.hooks.SessionStart(ctx, sessionID, "startup")
		if v.StopTurn {
			return "", firstNonEmptyStr(v.StopReason, "session blocked by SessionStart hook")
		}
		parts = appendNonEmpty(parts, v.AdditionalContext)
	}
	v := o.hooks.UserPromptSubmit(ctx, sessionID, prompt)
	if v.Blocked || v.StopTurn {
		return "", firstNonEmptyStr(v.Reason, v.StopReason, "prompt blocked by UserPromptSubmit hook")
	}
	return strings.Join(appendNonEmpty(parts, v.AdditionalContext), "\n"), ""
}

// withHookContext hook 附加上下文以显式标记块随任务意图下发，模型可区分用户原文与 hook 注入。
func withHookContext(input, hookContext string) string {
	if strings.TrimSpace(hookContext) == "" {
		return input
	}
	return input + "\n\n<hook-context>\n" + hookContext + "\n</hook-context>"
}

// continueOnStopHooks Stop hook 以 decision:block 要求继续时，先持久化当前回复，再以 hook 给出的
// 原因作为续跑指令重跑一轮内核；返回最终回复。
func (o *orchestrator) continueOnStopHooks(
	ctx context.Context, sink Sink, sessionID string, agentCtrl protocol.AgentController,
	history []types.Message, reply string,
) (string, string, bool) {
	for i := 0; i < maxStopContinuations; i++ {
		v := o.hooks.Stop(ctx, sessionID, reply, i > 0)
		if v.StopTurn || !v.Blocked || strings.TrimSpace(v.Reason) == "" {
			return reply, "", false
		}
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := o.persistence.SaveMessage(saveCtx, sessionID, "assistant", reply, "", "", 0); err != nil {
			slog.Error("session: saveMessage assistant before stop-hook continuation", "session", sessionID, "err", err)
		}
		cancel()
		history = append(history, types.Message{Role: "assistant", Content: reply})
		agentCtrl.SetConversationHistory(history)
		slog.Info("session: Stop hook requested continuation", "session", sessionID, "round", i+1)
		next, inferErr, aborted := o.runFSMTurn(ctx, sink, sessionID, agentCtrl,
			"<stop-hook>\n"+v.Reason+"\n</stop-hook>")
		if aborted || inferErr != "" || next == "" {
			return next, inferErr, aborted
		}
		reply = next
	}
	return reply, "", false
}

func appendNonEmpty(parts []string, s string) []string {
	if strings.TrimSpace(s) != "" {
		parts = append(parts, s)
	}
	return parts
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
