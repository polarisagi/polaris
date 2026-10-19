package agent

import (
	"context"

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
	msg := a.executeLoopToolCallRaw(ctx, call)
	if a.sCtx.LoopStreamsToUser() {
		a.publishStreamEvent(types.AgentStreamEvent{
			Type:       types.AgentStreamEventToolResult,
			ToolName:   call.Name,
			Content:    truncateStreamToolResult(msg.Content),
			TaintLevel: msg.TaintLevel,
		})
	}
	return msg
}
