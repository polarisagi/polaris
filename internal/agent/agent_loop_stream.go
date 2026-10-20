package agent

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/polarisagi/polaris/pkg/types"
)

// streamToolResultMaxChars 推给用户的工具结果事件内容上限。完整结果已在模型上下文中，
// 事件只是进度提示，不该把大输出整份塞进 SSE（R5-02）。
const streamToolResultMaxChars = 500

// truncateStreamToolResult 按字符（rune）截断，避免切断多字节字符。
func truncateStreamToolResult(s string) string {
	r := []rune(s)
	if len(r) <= streamToolResultMaxChars {
		return s
	}
	return string(r[:streamToolResultMaxChars]) + "..."
}

// executeLoopToolCall 执行单条工具调用，并在交互回合把结果以 ToolResult 事件推给用户（R5-02）。
// 工具调用事件已由 acceptStreamToolCall 在流式阶段发布，这里补齐成对的结果事件；
// 污点等级随结果走，客户端据此决定展示方式。
func (a *Agent) executeLoopToolCall(ctx context.Context, call types.InferToolCall) types.Message {
	start := time.Now()
	msg := a.executeLoopToolCallRaw(ctx, call)
	if a.sCtx.LoopStreamsToUser() {
		a.publishStreamEvent(types.AgentStreamEvent{
			Type:       types.AgentStreamEventToolResult,
			ToolName:   call.Name,
			Content:    truncateStreamToolResult(msg.Content),
			TaintLevel: msg.TaintLevel,
			CallID:     call.ID,
			ResultRaw:  capStreamResultRaw(msg.Content),
			IsError:    strings.HasPrefix(msg.Content, "Error: "),
			DurationMs: time.Since(start).Milliseconds(),
		})
	}
	return msg
}

// streamResultRawMaxBytes 随事件带给 session 层生成摘要的原始结果上限。摘要只读前部结构
// （搜索结果标题、网页标题、命令退出码与末尾输出），完整结果已在模型上下文中，不必整份搬运。
const streamResultRawMaxBytes = 64 << 10

func capStreamResultRaw(s string) string {
	if len(s) <= streamResultRawMaxBytes {
		return s
	}
	// 按字节截断后修正到 UTF-8 边界，避免把多字节字符切成乱码。
	cut := streamResultRawMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
