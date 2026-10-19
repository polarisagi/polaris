package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// makeToolResultMessage 构造符合 LLM tool_result 规范的消息结构体，支持文本与多模态图片内容块。
func makeToolResultMessage(callID string, content string, imageParts ...types.ImagePart) types.Message {
	if len(imageParts) == 0 {
		return types.Message{
			Role:    "user",
			Content: content,
			Parts: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": callID,
					"content":     content,
				},
			},
		}
	}

	// 多模态结果：将文本与图片块组装到 tool_result.content 块列表中（Anthropic/Vision 协议标准）
	blocks := make([]any, 0, 1+len(imageParts))
	if content != "" {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": content,
		})
	}
	for _, img := range imageParts {
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": img.MediaType,
				"data":       base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}

	return types.Message{
		Role:    "user",
		Content: content,
		Parts: []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": callID,
				"content":     blocks,
			},
		},
	}
}

// executeLoopToolCall 执行单条工具调用，并处理能力令牌与结果投影。
func (a *Agent) executeLoopToolCall(ctx context.Context, call types.InferToolCall) types.Message {
	if a.toolRegistry == nil {
		msg := makeToolResultMessage(call.ID, "Error: tool registry not configured")
		msg.Round = a.sCtx.LoopRound
		return msg
	}

	taskID := a.sCtx.SessionID
	toolCtx := context.WithValue(ctx, protocol.CtxTaskIDKey{}, taskID)
	toolCtx = context.WithValue(toolCtx, protocol.CtxModelSwitcherKey{}, a)
	toolCtx = context.WithValue(toolCtx, protocol.CtxSubagentManagerKey{}, a)

	// transfer_to_agent 在 agentic 模式下统一转调 SpawnAgents(wait=all)（ADR-0115 WP4 / R2-03）
	if call.Name == "transfer_to_agent" {
		return a.executeLoopTransferToAgent(toolCtx, call)
	}

	// JIT 签发能力令牌（必须在 validatedCalls 中），确保无旁路漏洞
	toolCtx, revoke, err := a.withJITCapability(toolCtx, call.Name, call.Input)
	if err != nil {
		slog.WarnContext(ctx, "agentic_loop: capability token mint failed", "tool", call.Name, "err", err)
		msg := makeToolResultMessage(call.ID, "Error: "+err.Error())
		msg.Round = a.sCtx.LoopRound
		return msg
	}
	defer revoke()

	sCtxMu := &a.sCtx.Mu
	sCtxMu.RLock()
	globalTaint := a.sCtx.GlobalTaintLevel
	currentRound := a.sCtx.LoopRound
	sCtxMu.RUnlock()

	res, err := a.toolRegistry.ExecuteWithTaint(toolCtx, call.Name, call.Input, globalTaint)
	if err != nil {
		slog.WarnContext(ctx, "agentic_loop: tool execution error", "tool", call.Name, "err", err)
		msg := makeToolResultMessage(call.ID, "Error: "+err.Error())
		msg.Round = currentRound
		return msg
	}

	if res == nil {
		msg := makeToolResultMessage(call.ID, "[Success (Empty Output)]")
		msg.Round = currentRound
		return msg
	}

	// P4: 工具结果污点加锁并入 GlobalTaintLevel（只升不降，ADR-0007）
	if res.TaintLevel > types.TaintNone {
		a.elevateGlobalTaintLevel(res.TaintLevel)
	}

	if res.Error != "" {
		msg := makeToolResultMessage(call.ID, "Error: "+res.Error)
		msg.Round = currentRound
		msg.TaintLevel = res.TaintLevel
		return msg
	}

	// 投影输出：超限时卸载到工作区并给出 read_tool_ref
	raw := res.Output
	rendered := raw
	if len(raw) > maxExecResultBytes {
		view := a.spillExecResult(toolCtx, raw)
		rendered = view.render(maxExecResultBytes)
	}

	a.appendExecuteImageParts(res.ImageParts)

	msg := makeToolResultMessage(call.ID, string(rendered), res.ImageParts...)
	msg.Round = currentRound
	msg.TaintLevel = res.TaintLevel
	return msg
}

// executeToolCallsConcurrently 执行多个独立的工具调用，并按原序收集结果。
// 依据 isConcurrencySafe 区分并发安全性（R2-06）：
// 按原顺序把连续的并发安全调用合成一组并行执行，其余调用按顺序串行执行。
func (a *Agent) executeToolCallsConcurrently(ctx context.Context, calls []types.InferToolCall) []types.Message {
	results := make([]types.Message, len(calls))
	if len(calls) == 0 {
		return results
	}
	if len(calls) == 1 {
		metrics.RecordLoopToolParallel(ctx, "serial", 1)
		results[0] = a.executeLoopToolCall(ctx, calls[0])
		return results
	}

	type toolCallItem struct {
		index int
		call  types.InferToolCall
	}
	type callBatch struct {
		safe  bool
		items []toolCallItem
	}

	var batches []callBatch
	for i, c := range calls {
		safe := a.isCallConcurrencySafe(c)
		if safe {
			if len(batches) > 0 && batches[len(batches)-1].safe {
				batches[len(batches)-1].items = append(batches[len(batches)-1].items, toolCallItem{index: i, call: c})
			} else {
				batches = append(batches, callBatch{
					safe:  true,
					items: []toolCallItem{{index: i, call: c}},
				})
			}
		} else {
			batches = append(batches, callBatch{
				safe:  false,
				items: []toolCallItem{{index: i, call: c}},
			})
		}
	}

	for _, batch := range batches {
		if batch.safe && len(batch.items) > 1 {
			metrics.RecordLoopToolParallel(ctx, "parallel", int64(len(batch.items)))
			var wg sync.WaitGroup
			wg.Add(len(batch.items))
			for _, it := range batch.items {
				item := it
				concurrent.SafeGo(ctx, "agentic_loop_tool_call", func(subCtx context.Context) {
					defer wg.Done()
					results[item.index] = a.executeLoopToolCall(subCtx, item.call)
				})
			}
			wg.Wait()
		} else {
			for _, it := range batch.items {
				metrics.RecordLoopToolParallel(ctx, "serial", 1)
				results[it.index] = a.executeLoopToolCall(ctx, it.call)
			}
		}
	}

	return results
}

// isCallConcurrencySafe 判断指定工具调用是否满足并发安全条件。
func (a *Agent) isCallConcurrencySafe(call types.InferToolCall) bool {
	if isDelegationTool(call.Name) {
		return false
	}
	tool, ok := a.toolMeta(call.Name)
	if !ok {
		return false
	}
	return isConcurrencySafe(tool)
}

// isConcurrencySafe 判定工具是否具备并发安全性（R2-06）。
// 契约：Capability == CapReadOnly，且 SideEffects 为空或只有 SideNone，
// 且不是 MCP、skill、code_act 来源（这些来源的元数据不可信，fail-closed）。
func isConcurrencySafe(tool types.Tool) bool {
	if tool.Capability != types.CapReadOnly {
		return false
	}
	for _, se := range tool.SideEffects {
		if se != types.SideNone && se != "" {
			return false
		}
	}
	if tool.Source == types.ToolMCP || tool.Source == types.ToolSkill || tool.Source == "code_act" ||
		strings.HasPrefix(tool.Name, "code_act:") || strings.HasPrefix(tool.Name, "skill__") ||
		strings.HasPrefix(tool.Name, "mcp__") || tool.Name == "code_act" || isDelegationTool(tool.Name) {
		return false
	}
	return true
}

// Steer 将用户在回合运行中发送的插话压入插话队列，并在下一轮边界注入。
func (a *Agent) Steer(text string) bool {
	if text == "" {
		return false
	}
	a.sCtx.Mu.Lock()
	a.sCtx.LoopSteerQueue = append(a.sCtx.LoopSteerQueue, text)
	a.sCtx.Mu.Unlock()

	// 广播 steer_accepted SSE 事件
	a.broadcastStreamEvent(types.AgentStreamEvent{
		Type:    types.AgentStreamEventSteerAccepted,
		Content: text,
	})
	return true
}

// SetModelLock 锁定用户手选的模型引用，在当前 Agent 会话中覆盖默认模型。
func (a *Agent) SetModelLock(ref string) {
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()
	a.sCtx.UserLockedModel = ref
}

// drainSteerQueue 出队当前所有插话消息。
func (a *Agent) drainSteerQueue() []string {
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()
	if len(a.sCtx.LoopSteerQueue) == 0 {
		return nil
	}
	steers := make([]string, len(a.sCtx.LoopSteerQueue))
	copy(steers, a.sCtx.LoopSteerQueue)
	a.sCtx.LoopSteerQueue = nil
	return steers
}

// broadcastStreamEvent 向所有流式订阅者分发事件。
func (a *Agent) broadcastStreamEvent(ev types.AgentStreamEvent) {
	a.streamSubsMu.Lock()
	defer a.streamSubsMu.Unlock()
	for _, ch := range a.streamSubs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// appendExecuteImageParts 向 sCtx.ExecuteImageParts 安全追加图片并执行单回合上限截断（V10）。
func (a *Agent) appendExecuteImageParts(parts []types.ImagePart) {
	if len(parts) == 0 {
		return
	}
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()
	cur := len(a.sCtx.ExecuteImageParts)
	if cur >= maxExecuteImageParts {
		slog.Warn("agent: execute image parts limit reached, discarding new images",
			"limit", maxExecuteImageParts, "discarded", len(parts))
		return
	}
	remaining := maxExecuteImageParts - cur
	if len(parts) > remaining {
		slog.Warn("agent: execute image parts truncated to limit",
			"limit", maxExecuteImageParts, "kept", remaining, "discarded", len(parts)-remaining)
		parts = parts[:remaining]
	}
	a.sCtx.ExecuteImageParts = append(a.sCtx.ExecuteImageParts, parts...)
}

// executeLoopTransferToAgent 处理 agentic 循环下的 transfer_to_agent 委派调用，统一转调 SpawnAgents(wait=all)。
func (a *Agent) executeLoopTransferToAgent(ctx context.Context, call types.InferToolCall) types.Message {
	currentRound := a.sCtx.LoopRound
	if a.handoffPoster == nil {
		msg := makeToolResultMessage(call.ID, "Error: transfer_to_agent: handoffPoster not injected (fail-closed)")
		msg.Round = currentRound
		return msg
	}

	var handoffArgs struct {
		TargetAgentRole string `json:"target_agent_role"`
		ContextSummary  string `json:"context_summary"`
	}
	if err := json.Unmarshal(call.Input, &handoffArgs); err != nil {
		msg := makeToolResultMessage(call.ID, "Error: transfer_to_agent: unmarshal args: "+err.Error())
		msg.Round = currentRound
		return msg
	}
	if handoffArgs.TargetAgentRole == "" {
		msg := makeToolResultMessage(call.ID, "Error: transfer_to_agent: target_agent_role is required")
		msg.Round = currentRound
		return msg
	}

	targetRole := handoffArgs.TargetAgentRole
	contextSummary := handoffArgs.ContextSummary
	if strings.HasPrefix(targetRole, mcpA2ATargetPrefix) {
		contextSummary = a.redactForMCPEgress(ctx, targetRole, contextSummary)
	}

	childIDs, err := a.SpawnAgents(ctx, types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{
			{
				Role:   targetRole,
				Prompt: contextSummary,
			},
		},
		Wait: "all",
	})
	if err != nil {
		msg := makeToolResultMessage(call.ID, "Error: transfer_to_agent: "+err.Error())
		msg.Round = currentRound
		return msg
	}

	msg := makeToolResultMessage(call.ID, fmt.Sprintf("dispatched to %s (task: %v)", targetRole, childIDs))
	msg.Round = currentRound
	return msg
}
