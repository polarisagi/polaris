package chat

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
)

// slashCommandMeta 命令元数据，用于 /help 输出和路由表。
type slashCommandMeta struct {
	Name string
	Desc string
}

func builtinSlashCommands() []slashCommandMeta {
	return []slashCommandMeta{
		{"/context", "显示当前上下文 token 使用量与压缩统计"},
		{"/compact", "立即压缩上下文（无论是否达到阈值）"},
		{"/clear", "清空当前会话历史（保留系统提示词）"},
		{"/help", "显示所有可用斜线命令"},
	}
}

// CommandResult 命令执行结果，由 SlashCommandRouter.Dispatch 返回。
type CommandResult struct {
	// Handled=true 表示命令已处理，调用方应短路（跳过 LLM 推理）。
	Handled bool
	// Response 是助手回复文本，非空时由调用方持久化到 DB。
	Response string
	// UpdatedHistory 是命令执行后的消息历史（/compact 和 /clear 会修改）。
	UpdatedHistory []types.Message
}

// SlashCommandRouter 斜线命令路由器。
// 拦截 /context /compact /clear /help，在 LLM 推理前短路处理。
// 命令不消耗 TokenBudget，不进入 Agent FSM。
type SlashCommandRouter struct {
	compressor SessionCompressor
	chatRepo   protocol.ChatRepository
	writeSSE   func(http.ResponseWriter, http.Flusher, string, any)
}

func NewSlashCommandRouter(compressor SessionCompressor, chatRepo protocol.ChatRepository, writeSSE func(http.ResponseWriter, http.Flusher, string, any)) *SlashCommandRouter {
	return &SlashCommandRouter{compressor: compressor, chatRepo: chatRepo, writeSSE: writeSSE}
}

// Dispatch 若 input 为斜线命令则执行并返回 CommandResult（Handled=true）。
// 非斜线命令时返回 Handled=false，调用方继续正常 LLM 流程。
// SSE 回复（token 事件）由各子处理器直接写入；complete 事件由调用方发送。
func (r *SlashCommandRouter) Dispatch(
	ctx context.Context,
	input, sessionID string,
	history []types.Message,
	provider protocol.Provider,
	w http.ResponseWriter, flusher http.Flusher,
) CommandResult {
	cmd, _, ok := parseSlashCommand(input)
	if !ok {
		return CommandResult{Handled: false, UpdatedHistory: history}
	}

	switch cmd {
	case "/context":
		resp := r.handleContext(sessionID, history, w, flusher)
		return CommandResult{Handled: true, Response: resp, UpdatedHistory: history}
	case "/compact":
		resp, updated := r.handleCompact(ctx, sessionID, history, provider, w, flusher)
		return CommandResult{Handled: true, Response: resp, UpdatedHistory: updated}
	case "/clear":
		resp, updated := r.handleClear(ctx, sessionID, history, w, flusher)
		return CommandResult{Handled: true, Response: resp, UpdatedHistory: updated}
	case "/help":
		resp := r.handleHelp(w, flusher)
		return CommandResult{Handled: true, Response: resp, UpdatedHistory: history}
	default:
		msg := fmt.Sprintf("未知命令: %s，输入 /help 查看可用命令", cmd)
		r.writeSSEText(w, flusher, msg)
		return CommandResult{Handled: true, Response: msg, UpdatedHistory: history}
	}
}

// handleContext 输出当前上下文 token 使用统计。
func (r *SlashCommandRouter) handleContext(sessionID string, history []types.Message, w http.ResponseWriter, flusher http.Flusher) string {
	stats := r.compressor.Stats(history)
	var lastCompact string
	if stats.LastCompactAt.IsZero() {
		lastCompact = "从未"
	} else {
		lastCompact = stats.LastCompactAt.Format(time.DateTime)
	}

	resp := fmt.Sprintf(
		"**上下文统计** (session: `%s`)\n\n"+
			"| 项目 | 值 |\n|---|---|\n"+
			"| 当前 token 数 | %d |\n"+
			"| 压缩阈值 | %d |\n"+
			"| 使用率 | %.1f%% |\n"+
			"| 消息条数 | %d |\n"+
			"| 最近压缩 | %s |",
		sessionID,
		stats.TokenCount, stats.Threshold, stats.UsagePercent,
		stats.MessageCount, lastCompact,
	)
	r.writeSSEText(w, flusher, resp)
	return resp
}

// handleCompact 强制触发上下文压缩。
func (r *SlashCommandRouter) handleCompact(
	ctx context.Context,
	sessionID string,
	history []types.Message,
	provider protocol.Provider,
	w http.ResponseWriter, flusher http.Flusher,
) (string, []types.Message) {
	if provider == nil {
		msg := "无法压缩：未配置 LLM 厂商（压缩需要 LLM 生成摘要）"
		r.writeSSEText(w, flusher, msg)
		return msg, history
	}

	r.writeSSE(w, flusher, "status", map[string]any{"type": "compacting", "message": "正在压缩上下文..."})

	compacted, res, err := r.compressor.ForceCompact(ctx, sessionID, history, provider)
	if err != nil {
		msg := fmt.Sprintf("压缩失败: %v", err)
		slog.Warn("slash /compact: ForceCompact error", "session", sessionID, "err", err)
		r.writeSSEText(w, flusher, msg)
		return msg, history
	}
	if res.Skipped {
		msg := fmt.Sprintf("上下文内容较少（%d tokens），无需压缩", res.TokensBefore)
		r.writeSSEText(w, flusher, msg)
		return msg, history
	}

	r.writeSSE(w, flusher, "status", map[string]any{
		"type":          "compacted",
		"tokens_before": res.TokensBefore,
		"tokens_after":  res.TokensAfter,
		"message":       fmt.Sprintf("上下文已压缩：%d → %d tokens", res.TokensBefore, res.TokensAfter),
	})

	resp := fmt.Sprintf(
		"上下文压缩完成：%d tokens → %d tokens（节省 %d%%）",
		res.TokensBefore, res.TokensAfter,
		100-res.TokensAfter*100/res.TokensBefore,
	)
	r.writeSSEText(w, flusher, resp)
	return resp, compacted
}

// handleClear 清空当前会话历史（物理删除 DB 中的消息，保留 system 提示词）。
func (r *SlashCommandRouter) handleClear(
	ctx context.Context,
	sessionID string,
	history []types.Message,
	w http.ResponseWriter, flusher http.Flusher,
) (string, []types.Message) {
	if r.chatRepo != nil {
		err := r.chatRepo.ClearNonSystemMessages(ctx, sessionID)
		if err != nil {
			slog.Warn("slash /clear: db delete failed", "session", sessionID, "err", err)
		}
	}
	// 保留 history 中的 system 消息，供本轮回复继续使用
	var retained []types.Message
	for _, m := range history {
		if m.Role == "system" {
			retained = append(retained, m)
		}
	}

	resp := "会话历史已清空（system 提示词保留）"
	r.writeSSEText(w, flusher, resp)
	return resp, retained
}

// handleHelp 输出可用斜线命令列表。
func (r *SlashCommandRouter) handleHelp(w http.ResponseWriter, flusher http.Flusher) string {
	var sb strings.Builder
	sb.WriteString("**可用斜线命令**\n\n")
	for _, c := range builtinSlashCommands() {
		fmt.Fprintf(&sb, "- `%s` — %s\n", c.Name, c.Desc)
	}
	resp := strings.TrimRight(sb.String(), "\n")
	r.writeSSEText(w, flusher, resp)
	return resp
}

// parseSlashCommand 解析输入是否为斜线命令，返回 (cmd, args, ok)。
// 仅处理行首的 /xxx 模式，保证普通消息中的 URL 不被误判。
func parseSlashCommand(input string) (cmd, args string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", false
	}
	// 排除 URL：//foo 或 https:// 等
	if strings.HasPrefix(trimmed, "//") {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, " ", 2)
	cmd = strings.ToLower(parts[0])
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return cmd, args, true
}

// writeSSEText 以 token 事件流的形式发送纯文本回复。
// 不发送 complete 事件——由调用方统一负责。
func (r *SlashCommandRouter) writeSSEText(w http.ResponseWriter, flusher http.Flusher, text string) {
	r.writeSSE(w, flusher, "token", map[string]string{"content": text})
}
