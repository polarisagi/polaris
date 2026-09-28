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
// 推理错误信息（如有）、是否因客户端中止而提前返回、本轮产生的 MCP Apps
// 视图 ID 列表（M8f-1，供调用方在 assistant 消息落库后回填关联）。
func (o *orchestrator) runFSMTurn(
	ctx context.Context,
	sink Sink,
	sessionID string,
	agentCtrl protocol.AgentController,
	input string,
) (reply string, inferErr string, aborted bool, viewIDs []string) {
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
		return "", "Agent 状态机未能接收本轮输入，请稍后重试", false, nil
	}

	var replyBuilder []byte
	var errBuilder string
	// [HE-1] 回合终止方式此前完全不可观测：channel 关闭、task_done、ctx 取消
	// 三条出口在"回复为空且无错误"时表现完全一致，日志里只剩一条无法定位的
	// "推理返回空内容"。记录事件计数与出口分支，使空回合可归因。
	evCount := 0

	windowSize := config.CurrentThresholds().Session.LeakScanWindowBytes
	if windowSize <= 0 {
		windowSize = 20
	}
	var leakWindow []byte

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				slog.WarnContext(ctx, "session: fsm stream closed without task_done",
					"session", sessionID, "events", evCount, "reply_bytes", len(replyBuilder), "infer_err", errBuilder)
				return string(replyBuilder), errBuilder, false, viewIDs
			}
			evCount++
			stop := o.handleFSMEvent(ctx, sink, sessionID, ev, systemPromptGuard, &replyBuilder, &errBuilder, &leakWindow, windowSize, &viewIDs)
			if stop {
				slog.InfoContext(ctx, "session: fsm turn done",
					"session", sessionID, "events", evCount, "reply_bytes", len(replyBuilder), "infer_err", errBuilder)
				return string(replyBuilder), errBuilder, false, viewIDs
			}
		case <-ctx.Done():
			// [GD-13-002] 客户端断连时通知 Agent Kernel 强制中止，避免后台无感空跑
			if agentCtrl != nil {
				agentCtrl.Interrupt(types.InterruptRequest{Action: types.InterruptAbort})
			}
			return string(replyBuilder), ctx.Err().Error(), true, viewIDs
		}
	}
}

// handleFSMEvent 处理单条 FSM 流事件：按类型转发领域事件并累积 reply/inferErr
// （原 chat/sse_stream_helpers.go handleStreamFSMEvent 迁入，行为不变）。
// 返回 stop=true 时调用方应结束事件循环（task_done 状态事件）。
func (o *orchestrator) handleFSMEvent( //nolint:gocyclo
	ctx context.Context,
	sink Sink,
	sessionID string,
	ev types.AgentStreamEvent,
	systemPromptGuard *guard.SystemPromptGuard,
	reply *[]byte,
	inferErr *string,
	leakWindow *[]byte,
	windowSize int,
	viewIDs *[]string,
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
		if ev.UI != nil {
			o.handleToolUIEvent(ctx, sink, sessionID, ev.UI, viewIDs)
		}
	case types.AgentStreamEventError:
		if *inferErr == "" {
			*inferErr = ev.Content
		}
		o.emitError(sink, "fsm_error", ev.Content, sessionID, nil)
	case types.AgentStreamEventPhase:
		// 回合阶段进度（ADR-0098）：内部阶段不再推 token，客户端靠它在首 token 前
		// 展示进度。阶段键原样透传，本地化由客户端负责。
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{"type": "phase", "phase": ev.Content}})
	case types.AgentStreamEventApproval:
		// 回合内阻塞式人工审批：客户端就地渲染审批卡片。走 KindStatus，不进 *reply。
		_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{
			"type":        "approval_required",
			"id":          ev.Content,
			"tool":        ev.ToolName,
			"input":       string(ev.ToolInput),
			"deadline_ns": ev.DeadlineNs,
		}})
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

// linkAppViews 把本轮产生的视图关联到刚落库的 assistant 消息（M8f-1）。
// messageID<=0（写入失败/降级到 outbox 异步兜底，见 Persistence.SaveMessage 注释）
// 或 viewIDs 为空时跳过——都不是错误，只是本轮没有可关联的东西。
func (o *orchestrator) linkAppViews(ctx context.Context, sessionID string, viewIDs []string, messageID int64) {
	if len(viewIDs) == 0 || messageID <= 0 {
		return
	}
	if err := o.persistence.LinkAppViewsToMessage(ctx, sessionID, viewIDs, messageID); err != nil {
		slog.Error("session: link app views to message failed", "session", sessionID, "message_id", messageID, "err", err)
	}
}

// handleToolUIEvent 落库一次 MCP Apps 视图快照并推送 KindStatus "tool_ui"
// 事件（M8f-1）。落库失败只记 Warn 不阻断本轮——前端仍能实时渲染视图，只是
// 刷新页面后这一条可能取不到历史记录，比整轮失败更符合"能用先用"的降级原则。
// viewIDs 只在落库成功时追加：LinkAppViewsToMessage 按 view_id 匹配回填，
// 数据库里不存在的 id 回填是无意义的空操作。
func (o *orchestrator) handleToolUIEvent(ctx context.Context, sink Sink, sessionID string, ui *types.ToolUIRef, viewIDs *[]string) {
	err := o.persistence.SaveAppView(ctx, AppView{
		ViewID:      ui.ViewID,
		ServerID:    ui.ServerID,
		ResourceURI: ui.ResourceURI,
		ToolName:    ui.ToolName,
		ToolInput:   ui.ToolInput,
		ToolResult:  ui.ToolResult,
	})
	if err != nil {
		slog.Warn("session: save app view failed", "session", sessionID, "view_id", ui.ViewID, "err", err)
	} else {
		*viewIDs = append(*viewIDs, ui.ViewID)
	}
	_ = sink.Emit(Event{Kind: KindStatus, Payload: map[string]any{
		"type":         "tool_ui",
		"view_id":      ui.ViewID,
		"server_id":    ui.ServerID,
		"resource_uri": ui.ResourceURI,
		"tool_name":    ui.ToolName,
		"tool_input":   ui.ToolInput,
		"tool_result":  ui.ToolResult,
		"cancelled":    ui.Cancelled,
	}})
}
