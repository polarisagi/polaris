package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/memory/compact"
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
		return makeToolResultMessage(call.ID, "Error: tool registry not configured")
	}

	taskID := a.sCtx.SessionID
	toolCtx := context.WithValue(ctx, protocol.CtxTaskIDKey{}, taskID)
	toolCtx = context.WithValue(toolCtx, protocol.CtxModelSwitcherKey{}, a)
	toolCtx = context.WithValue(toolCtx, protocol.CtxSubagentManagerKey{}, a)

	// JIT 签发能力令牌（必须在 validatedCalls 中），确保无旁路漏洞
	toolCtx, revoke, err := a.withJITCapability(toolCtx, call.Name, call.Input)
	if err != nil {
		slog.WarnContext(ctx, "agentic_loop: capability token mint failed", "tool", call.Name, "err", err)
		return makeToolResultMessage(call.ID, "Error: "+err.Error())
	}
	defer revoke()

	sCtxMu := &a.sCtx.Mu
	sCtxMu.RLock()
	globalTaint := a.sCtx.GlobalTaintLevel
	sCtxMu.RUnlock()

	res, err := a.toolRegistry.ExecuteWithTaint(toolCtx, call.Name, call.Input, globalTaint)
	if err != nil {
		slog.WarnContext(ctx, "agentic_loop: tool execution error", "tool", call.Name, "err", err)
		return makeToolResultMessage(call.ID, "Error: "+err.Error())
	}

	if res == nil {
		return makeToolResultMessage(call.ID, "[Success (Empty Output)]")
	}

	if res.Error != "" {
		return makeToolResultMessage(call.ID, "Error: "+res.Error)
	}

	// 投影输出：超限时卸载到工作区并给出 read_tool_ref
	raw := res.Output
	rendered := raw
	if len(raw) > maxExecResultBytes {
		view := a.spillExecResult(toolCtx, raw)
		rendered = view.render(maxExecResultBytes)
	}

	if len(res.ImageParts) > 0 {
		a.sCtx.Mu.Lock()
		a.sCtx.ExecuteImageParts = append(a.sCtx.ExecuteImageParts, res.ImageParts...)
		a.sCtx.Mu.Unlock()
	}

	return makeToolResultMessage(call.ID, string(rendered), res.ImageParts...)
}

// executeToolCallsConcurrently 并发执行多个独立的工具调用，并按原序收集结果。
func (a *Agent) executeToolCallsConcurrently(ctx context.Context, calls []types.InferToolCall) []types.Message {
	results := make([]types.Message, len(calls))
	if len(calls) == 0 {
		return results
	}
	if len(calls) == 1 {
		results[0] = a.executeLoopToolCall(ctx, calls[0])
		return results
	}

	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i, c := range calls {
		idx := i
		callItem := c
		concurrent.SafeGo(ctx, "agentic_loop_tool_call", func(subCtx context.Context) {
			defer wg.Done()
			results[idx] = a.executeLoopToolCall(subCtx, callItem)
		})
	}
	wg.Wait()
	return results
}

// applyLoopObservationMasking 执行 T3 批量观察遮蔽：
// 超过 observation_mask_after_rounds 的工具输出按批替换为 read_tool_ref 引用，保证批量驱逐点之间前缀稳定。
func (a *Agent) applyLoopObservationMasking(ctx context.Context) {
	a.sCtx.Mu.Lock()
	defer a.sCtx.Mu.Unlock()

	th := config.CurrentThresholds().M4Kernel
	maskAfter := a.sCtx.LoopObservationMaskAfterRounds
	if maskAfter <= 0 {
		maskAfter = th.LoopObservationMaskAfterRounds
	}
	if maskAfter <= 0 {
		maskAfter = 6
	}

	batchSize := a.sCtx.LoopObservationMaskBatch
	if batchSize <= 0 {
		batchSize = th.LoopObservationMaskBatch
	}
	if batchSize <= 0 {
		batchSize = 4
	}

	// 收集待遮蔽的工具结果下标
	candidateIndices := collectMaskCandidates(a.sCtx.LoopMessages, a.sCtx.LoopRound, maskAfter)

	// 只有达到批量大小 batchSize 时才触发批量替换，确保批驱逐点之间前缀字节完全不变
	if len(candidateIndices) < batchSize {
		return
	}

	taskID := a.memoryPartitionKey()
	for _, idx := range candidateIndices[:batchSize] {
		maskSingleMessage(ctx, a.toolOffloader, taskID, &a.sCtx.LoopMessages[idx])
	}

	metrics.RecordObservationMasked(ctx, int64(batchSize))
	slog.InfoContext(ctx, "agentic_loop: batch observation masking applied",
		"session", a.sCtx.SessionID, "masked_count", batchSize)
}

// collectMaskCandidates 收集距离当前轮次超过 maskAfter 轮的工具结果下标。
func collectMaskCandidates(messages []types.Message, currentRound, maskAfter int) []int {
	var candidates []int
	for i, m := range messages {
		if !isToolResultMessage(m) {
			continue
		}
		if strings.HasPrefix(m.Content, "[output masked: full output: read_tool_ref") {
			continue
		}
		if currentRound-1 >= maskAfter {
			candidates = append(candidates, i)
		}
	}
	return candidates
}

// maskSingleMessage 将单条工具消息的内容及 Parts 替换为 read_tool_ref 掩码引用。
func maskSingleMessage(ctx context.Context, offloader compact.Offloader, taskID string, targetMsg *types.Message) {
	contentBytes := []byte(targetMsg.Content)
	var refID string
	if offloader != nil {
		if id, err := offloader.Offload(ctx, taskID, contentBytes); err == nil {
			refID = id
		}
	}
	if refID == "" {
		sum := sha256.Sum256(contentBytes)
		refID = hex.EncodeToString(sum[:8])
	}
	maskText := fmt.Sprintf("[output masked: full output: read_tool_ref(task_id=%q, id=%q)]", taskID, refID)
	targetMsg.Content = maskText
	for pi, p := range targetMsg.Parts {
		if partMap, ok := p.(map[string]any); ok && partMap["type"] == "tool_result" {
			newPart := make(map[string]any, len(partMap))
			for k, v := range partMap {
				newPart[k] = v
			}
			newPart["content"] = maskText
			targetMsg.Parts[pi] = newPart
		}
	}
}

// isToolResultMessage 判断消息是否为工具结果消息。
func isToolResultMessage(m types.Message) bool {
	if m.Role == "tool" {
		return true
	}
	for _, p := range m.Parts {
		if partMap, ok := p.(map[string]any); ok {
			if partMap["type"] == "tool_result" {
				return true
			}
		}
	}
	return false
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
