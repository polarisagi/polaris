package session

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// runFSMTurn / handleFSMEvent 从 orchestrator_interactive.go 拆出（A-03 Step7，
// Test_inv_FileLineLimit R7 400 行治理——原文件 404 行，超限 4 行；FSM 事件流
// 消费是与 runInteractive 主编排流程逻辑独立的子职责，拆分不改变任何行为）。

// runFSMTurn 驱动一轮 FSM 推理并把事件流转译为领域事件推送给 sink（原
// chat/sse.go handleAgentStreamFSM 迁入，行为不变）。返回值：聚合回复文本、
// 推理错误信息（如有）、是否因客户端中止而提前返回。
func (o *orchestrator) runFSMTurn(
	ctx context.Context,
	sink Sink,
	sessionID string,
	agentCtrl protocol.AgentController,
	input string,
) (reply string, inferErr string, aborted bool) {
	// [W-2-A] 接入 SystemPromptGuard——同时注册 FSM 内核阶段模板（静态指令主体）
	// 与 ActivatedSystemPrompt（M9 GEPA 动态激活提示词，可能为空），覆盖两类
	// "系统提示词"来源，不只挡后者。
	systemPromptGuard := newTurnSystemPromptGuard(o.prompt.ReadActivatedSystemPrompt())

	// 先订阅后触发：订阅通道就绪前 FSM 不会开始产出，消除早期事件丢失竞态。
	ch := agentCtrl.SubscribeStream(ctx)

	userTS := taint.NewTaintedString(input, taint.TaintSource{
		Module:           "gateway/chat",
		EventID:          sessionID,
		OriginTaintLevel: types.TaintHigh,
	}, "http_gateway")
	agentCtrl.SetTaskIntent(userTS)
	if err := agentCtrl.SendIntent(types.TriggerIntentReceived); err != nil {
		slog.Warn("session: fsm advance failed or timeout", "err", err)
		return "", "Agent 状态机未能接收本轮输入，请稍后重试", false
	}

	var replyBuilder []byte
	var errBuilder string

	windowSize := config.Get().Thresholds.Session.LeakScanWindowBytes
	if windowSize <= 0 {
		windowSize = 20
	}
	var leakWindow []byte

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return string(replyBuilder), errBuilder, false
			}
			stop := o.handleFSMEvent(sink, sessionID, ev, systemPromptGuard, &replyBuilder, &errBuilder, &leakWindow, windowSize)
			if stop {
				return string(replyBuilder), errBuilder, false
			}
		case <-ctx.Done():
			// [GD-13-002] 客户端断连时通知 Agent Kernel 强制中止，避免后台无感空跑
			if agentCtrl != nil {
				agentCtrl.Interrupt(types.InterruptRequest{Action: types.InterruptAbort})
			}
			return string(replyBuilder), ctx.Err().Error(), true
		}
	}
}

// handleFSMEvent 处理单条 FSM 流事件：按类型转发领域事件并累积 reply/inferErr
// （原 chat/sse_stream_helpers.go handleStreamFSMEvent 迁入，行为不变）。
// 返回 stop=true 时调用方应结束事件循环（task_done 状态事件）。
func (o *orchestrator) handleFSMEvent( //nolint:gocyclo
	sink Sink,
	sessionID string,
	ev types.AgentStreamEvent,
	systemPromptGuard *guard.SystemPromptGuard,
	reply *[]byte,
	inferErr *string,
	leakWindow *[]byte,
	windowSize int,
) (stop bool) {
	// GD-13-001：处理子 Agent 嵌套事件，添加角色前缀
	prefix := ""
	if ev.IsNested && ev.ChildAgentRole != "" {
		prefix = fmt.Sprintf("[%s] ", ev.ChildAgentRole)
		if ev.Content != "" && ev.Type != types.AgentStreamEventToken {
			ev.Content = prefix + ev.Content
		}
	}

	switch ev.Type {
	case types.AgentStreamEventThinking:
		_ = sink.Emit(Event{Kind: KindReasoning, Text: ev.Content})
	case types.AgentStreamEventToken:
		fullText := string(*leakWindow) + ev.Content
		_, err := systemPromptGuard.Scan(fullText, false)
		cleaned, _ := systemPromptGuard.Scan(ev.Content, true)
		if err != nil {
			slog.Warn("session: system prompt leak detected", "session_id", sessionID, "err", err)
			cleaned = ""      // 命中则清理本 token 可输出部分
			*leakWindow = nil // 重置窗口，防止后续正常 token 一直被误判
		} else {
			*leakWindow = append(*leakWindow, ev.Content...)
			if len(*leakWindow) > windowSize {
				*leakWindow = (*leakWindow)[len(*leakWindow)-windowSize:]
			}
		}
		_ = sink.Emit(Event{Kind: KindDelta, Text: cleaned})
		*reply = append(*reply, cleaned...)
	case types.AgentStreamEventToolCall:
		msg := fmt.Sprintf("%sExecuting tool %s...", prefix, ev.ToolName)
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{"type": "tool_call", "message": msg}})
	case types.AgentStreamEventToolResult:
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{"type": "tool_result", "message": ev.Content}})
	case types.AgentStreamEventError:
		if *inferErr == "" {
			*inferErr = ev.Content
		}
		o.emitError(sink, "fsm_error", ev.Content, sessionID, nil)
	case types.AgentStreamEventNotice:
		// 系统旁路提示（当前唯一来源：LLM 跨 Model Pool 降级，GD-13-005）。
		// 走 KindStatus 而非 KindDelta——它不是模型输出，绝不能进 *reply
		// （那会被当作助手回复正文持久化进消息历史）。
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{"type": "notice", "message": ev.Content}})
	case types.AgentStreamEventStatus:
		if ev.Content == "task_done" {
			return true
		}
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{"type": "info", "message": ev.Content}})
	}
	return false
}
