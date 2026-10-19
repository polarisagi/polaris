package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/config"
	compact "github.com/polarisagi/polaris/internal/memory/compact"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

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
		if currentRound-m.Round >= maskAfter {
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
