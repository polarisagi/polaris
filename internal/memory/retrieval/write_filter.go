package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/memory/store"

	"github.com/polarisagi/polaris/internal/protocol"
)

const (
	// writeFilterThreshold 写入阈值：information_value < threshold → 跳过写入 semantic_entities。
	// 阈值来源：MemReader 2604.07877 §4 实验结论（0.4 在 LoCoMo 上 F1 最优）。
	writeFilterThreshold = 0.4

	// writeFilterTimeout LLM 评估超时，超时走启发式 fallback。
	writeFilterTimeout = 5 * time.Second
)

// WriteFilter 写入前主动价值评估器。
// 主路径：调用 LLM（DeepSeek V4）评估信息价值。
// 回退：启发式评分（provider=nil 或超时时）。
// 安全约束：TaintLevel >= store.TaintCritical → 强制跳过，不评估。
type WriteFilter struct {
	provider       protocol.Provider
	writeThreshold float64
}

// NewWriteFilter 创建 WriteFilter。provider 可为 nil（走启发式回退）。
func NewWriteFilter(provider protocol.Provider) *WriteFilter {
	return &WriteFilter{
		provider:       provider,
		writeThreshold: writeFilterThreshold,
	}
}

// NewWriteFilterWithThreshold 允许覆盖阈值（测试/运维专用）。
func NewWriteFilterWithThreshold(provider protocol.Provider, threshold float64) *WriteFilter {
	return &WriteFilter{provider: provider, writeThreshold: threshold}
}

// EvalResult 评估结果。
type EvalResult struct {
	Value      float64 // 0.0-1.0
	ShouldSkip bool    // true = 跳过写入
	Reason     string  // 跳过原因（日志用）
}

// Evaluate 评估一段内容是否值得写入语义记忆。
//
// taintLevel: 内容污点级别（ADR-0007）。
// existingCount: 当前语义记忆中已有的相关实体数（供 LLM 参考）。
func (f *WriteFilter) Evaluate(
	ctx context.Context,
	content string,
	taintLevel int,
	existingCount int,
) EvalResult {
	// 安全门：store.TaintCritical 内容禁止写入语义记忆（可能含敏感系统信息）
	if taintLevel >= store.TaintCritical {
		return EvalResult{Value: 0, ShouldSkip: true, Reason: "taint_critical"}
	}

	if f.provider != nil {
		if result, err := f.llmEvaluate(ctx, content, existingCount); err == nil {
			result.ShouldSkip = result.Value < f.writeThreshold
			return result
		}
	}

	// 启发式回退
	return f.heuristicEvaluate(content)
}

// llmEvaluate 调用 LLM 评估内容价值。
func (f *WriteFilter) llmEvaluate(
	ctx context.Context,
	content string,
	existingCount int,
) (EvalResult, error) {
	ctx, cancel := context.WithTimeout(ctx, writeFilterTimeout)
	defer cancel()

	prompt := fmt.Sprintf(
		"You are evaluating whether the following information is worth storing in an AI agent's long-term semantic memory.\n\n"+
			"Criteria:\n"+
			"1. Novelty: Is this information new compared to what's likely known? (existing_facts=%d)\n"+
			"2. Utility: Will this likely be useful in future conversations?\n"+
			"3. Specificity: Is this a concrete fact vs. generic filler?\n\n"+
			"Return ONLY valid JSON: {\"score\": <0.0-1.0>, \"reason\": \"<one sentence>\"}\n\n"+
			"Content to evaluate:\n%s",
		existingCount,
		truncate(content, 500),
	)

	resp, err := safecall.Infer(ctx, f.provider,
		[]types.Message{{Role: "user", Content: prompt}},
		types.WithMaxTokens(64),
	)
	if err != nil {
		return EvalResult{}, apperr.Wrap(apperr.CodeInternal, "write_filter: provider infer", err)
	}

	text := strings.TrimSpace(resp.Content)
	if idx := strings.Index(text, "{"); idx > 0 {
		text = text[idx:]
	}

	var result struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return EvalResult{}, apperr.Wrap(apperr.CodeInternal, "write_filter: parse llm response", err)
	}

	if result.Score < 0 {
		result.Score = 0
	}
	if result.Score > 1 {
		result.Score = 1
	}

	return EvalResult{Value: result.Score, Reason: result.Reason}, nil
}

// heuristicEvaluate 启发式评分（provider=nil 或 LLM 超时时使用）。
// 评分维度: 内容长度 + 命名实体密度（大写词频率）+ 数字密度。
func (f *WriteFilter) heuristicEvaluate(content string) EvalResult {
	if len(strings.TrimSpace(content)) < 10 {
		return EvalResult{Value: 0.1, ShouldSkip: true, Reason: "too_short"}
	}

	words := strings.Fields(content)
	if len(words) == 0 {
		return EvalResult{Value: 0.1, ShouldSkip: true, Reason: "empty"}
	}

	var caps, nums int
	for _, w := range words {
		runes := []rune(w)
		if len(runes) > 1 && unicode.IsUpper(runes[0]) {
			caps++
		}
		for _, r := range runes {
			if unicode.IsDigit(r) {
				nums++
				break
			}
		}
	}

	entityDensity := float64(caps) / float64(len(words))
	numDensity := float64(nums) / float64(len(words))
	raw := float64(len(words)) / 50.0
	lengthScore := raw
	if lengthScore > 0.5 {
		lengthScore = 0.5
	}

	score := lengthScore + entityDensity*0.3 + numDensity*0.2
	if score > 1.0 {
		score = 1.0
	}

	return EvalResult{
		Value:      score,
		ShouldSkip: score < f.writeThreshold,
		Reason:     "heuristic",
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
