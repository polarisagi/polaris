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

// applyLoopObservationMasking 执行 T3 观察遮蔽（ADR-0120 决策四修订为成本门控）：上下文达到触发阈值后，
// 把超过 observation_mask_after_rounds 且足够大的工具输出一次性替换为 read_tool_ref 引用。
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

	minCandidates := a.sCtx.LoopObservationMaskBatch
	if minCandidates <= 0 {
		minCandidates = th.LoopObservationMaskBatch
	}
	if minCandidates <= 0 {
		minCandidates = 1
	}

	// 成本门控（ADR-0120 决策四）：遮蔽改写的是历史中段，被遮蔽点之后的全部前缀随之失去缓存；
	// 命中价约为未命中价 1/50，只有上下文已经很大时，省下的 token 才抵得过一次重建缓存。
	if a.loopContextEstimateLocked() < a.maskTriggerTokens(th.LoopObservationMaskTriggerTokens) {
		return
	}
	minTokens := th.LoopObservationMaskMinTokens
	if minTokens <= 0 {
		minTokens = 1024
	}

	candidateIndices := collectMaskCandidates(a.sCtx.LoopMessages, a.sCtx.LoopRound, maskAfter, minTokens)
	if len(candidateIndices) < minCandidates {
		return
	}

	// 一次遮蔽全部候选：每次遮蔽都打断一次缓存，分批只会多打断几次。
	taskID := a.memoryPartitionKey()
	masked := 0
	for _, idx := range candidateIndices {
		masked += compact.RoughTokens(a.sCtx.LoopMessages[idx : idx+1])
		maskSingleMessage(ctx, a.toolOffloader, taskID, &a.sCtx.LoopMessages[idx])
	}

	metrics.RecordObservationMasked(ctx, int64(len(candidateIndices)))
	slog.InfoContext(ctx, "agentic_loop: observation masking applied",
		"session", a.sCtx.SessionID, "masked_count", len(candidateIndices),
		"masked_tokens", masked, "prefix_break", "observation_mask")
}

// maskTriggerTokens 遮蔽触发阈值：配置显式值 > 上下文窗口的 50% > 64000。
func (a *Agent) maskTriggerTokens(configured int) int {
	if configured > 0 {
		return configured
	}
	if a.cwm != nil && a.cwm.MaxTokens() > 0 {
		return a.cwm.MaxTokens() / 2
	}
	return 64000
}

// loopContextEstimateLocked 估计当前请求的上下文规模：优先取上一次请求的整段估计（含前缀与历史），
// 没有时退回回合消息本身。调用方持有 sCtx.Mu。
func (a *Agent) loopContextEstimateLocked() int {
	est := compact.RoughTokens(a.sCtx.LoopMessages)
	if a.cwm != nil {
		est = max(est, a.cwm.currentUsageTokens())
	}
	return est
}

func collectMaskCandidates(messages []types.Message, currentRound, maskAfter, minTokens int) []int {
	var candidates []int
	for i, m := range messages {
		if !isToolResultMessage(m) {
			continue
		}
		if strings.HasPrefix(m.Content, "[output masked: full output: read_tool_ref") {
			continue
		}
		if currentRound-m.Round < maskAfter {
			continue
		}
		if compact.RoughTokens(messages[i:i+1]) < minTokens {
			continue
		}
		candidates = append(candidates, i)
	}
	return candidates
}

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
