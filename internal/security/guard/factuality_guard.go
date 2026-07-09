package guard

import (
	"github.com/polarisagi/polaris/internal/security/policy"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"math/rand"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// FactualityGuard D6 防线——LLM 输出真实性核验。
// 架构文档: docs/arch/M11-Policy-Safety.md §6.5
//
// D6 与 D1~D5 互补：在 LLM 输出后、出口前执行内容真实性核验。
// 三层核验:
//
//	L1 CitationCheck:        引用存在性检查（引用的事实/数字是否在 context 中有出处）
//	L2 NumericalConsistency: 数值一致性检查（输出数值与已知约束对比）
//	L3 SemanticJudge:        语义判断（LLM 自我核验，仅高风险输出触发；Tier0=pass-through）
//
// 采样率: TaintHigh 内容 1.0（必须核验），其余 0.1（抽样核验）。
// 结果路由:
//
//	FactualityPass      → 继续传递
//	FactualityUncertain → 低置信度标记，不阻断
//	FactualityFail      → 降级（返回降级消息）+ OnFail 回调
type FactualityGuard struct {
	sampleRate  float64               // 非高污点内容的采样率（默认 0.1）
	OnFail      func(FactualityAlert) // 核验失败回调
	llmProvider protocol.Provider     // Tier1+：L3 LLM-as-Judge；nil 时 L3 pass-through
}

// InjectLLMProvider 注入 LLM Provider 用于 L3 SemanticJudge（Tier1+）。
func (fg *FactualityGuard) InjectLLMProvider(p protocol.Provider) { fg.llmProvider = p }

// FactualityVerdict 核验裁决。
type FactualityVerdict int

const (
	FactualityPass      FactualityVerdict = iota // 通过核验
	FactualityUncertain                          // 不确定（低置信度标记，不阻断）
	FactualityFail                               // 核验失败
)

// FactualityAlert 核验失败告警。
type FactualityAlert struct {
	FailedLayer string // "citation" | "numerical" | "semantic"
	Content     string // 触发失败的内容摘要
	Reason      string
}

// FactualityResult 核验结果。
type FactualityResult struct {
	Verdict FactualityVerdict
	Layer   string // 最后通过的层级
	Reason  string
}

// NewFactualityGuard 创建 D6 防线。
func NewFactualityGuard() *FactualityGuard {
	return &FactualityGuard{sampleRate: 0.1}
}

// Verify 对 LLM 输出执行三层核验。
// content:    LLM 输出文本
// contextDoc: 参考上下文（用于 CitationCheck）
// taintLevel: 内容污点等级（TaintHigh → 强制核验，其余抽样）
func (fg *FactualityGuard) Verify(
	ctx context.Context,
	content string,
	contextDoc string,
	taintLevel types.TaintLevel,
) (FactualityResult, error) {
	// 采样门控: 非高污点内容按概率抽查
	if taintLevel < types.TaintHigh {
		if rand.Float64() > fg.sampleRate { //nolint:gosec
			return FactualityResult{Verdict: FactualityPass, Layer: "sampled_skip"}, nil
		}
	}

	// L1 CitationCheck：引用存在性检查
	if v, reason := fg.citationCheck(content, contextDoc); v == FactualityFail {
		fg.emitFail("citation", content, reason)
		return FactualityResult{Verdict: FactualityFail, Layer: "citation", Reason: reason}, nil
	}

	// L2 NumericalConsistency：数值一致性检查
	if v, reason := fg.numericalCheck(content, contextDoc); v == FactualityFail {
		fg.emitFail("numerical", content, reason)
		return FactualityResult{Verdict: FactualityFail, Layer: "numerical", Reason: reason}, nil
	}

	// L3 SemanticJudge：仅对 TaintHigh 内容触发（Tier1+ LLM judge；Tier0 pass-through）
	if taintLevel >= types.TaintHigh && fg.llmProvider != nil {
		verdict, reason := fg.semanticJudge(ctx, content, contextDoc)
		if verdict == FactualityFail {
			fg.emitFail("semantic", content, reason)
			return FactualityResult{Verdict: FactualityFail, Layer: "semantic", Reason: reason}, nil
		}
		if verdict == FactualityUncertain {
			return FactualityResult{Verdict: FactualityUncertain, Layer: "semantic", Reason: reason}, nil
		}
	}

	return FactualityResult{Verdict: FactualityPass, Layer: "all"}, nil
}

// semanticJudge L3 语义核验：调用 LLM 检查 content 中的事实性声明是否与 contextDoc 一致。
// 超时或 LLM 故障 → FactualityUncertain（不阻断）。
func (fg *FactualityGuard) semanticJudge(ctx context.Context, content, contextDoc string) (FactualityVerdict, string) {
	judgeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	prompt := "You are a factuality checker. Determine if the CLAIM is factually consistent with the CONTEXT.\n\n" +
		"CONTEXT:\n" + truncate(contextDoc, 1000) + "\n\n" +
		"CLAIM:\n" + truncate(content, 500) + "\n\n" +
		"Reply with one word only: PASS, UNCERTAIN, or FAIL. Then optionally one sentence reason."
	req := &types.InferRequest{
		Messages:    []types.Message{{Role: "user", Content: prompt}},
		MaxTokens:   64,
		Temperature: 0,
	}
	//custom-nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := fg.llmProvider.Infer(judgeCtx, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil || resp == nil {
		// L3 Judge 不可用：计数告警，不阻断（FactualityUncertain）
		// 监控指标: polaris.factuality.judge_unavailable_total
		metrics.GlobalFactualityJudgeUnavailableTotal.Add(1)
		return FactualityUncertain, "llm_judge_unavailable"
	}
	upper := strings.ToUpper(strings.TrimSpace(resp.Content))
	switch {
	case strings.HasPrefix(upper, "FAIL"):
		parts := strings.SplitN(resp.Content, " ", 2)
		reason := "semantic_fail"
		if len(parts) > 1 {
			reason = strings.TrimSpace(parts[1])
		}
		return FactualityFail, reason
	case strings.HasPrefix(upper, "UNCERTAIN"):
		return FactualityUncertain, "semantic_uncertain"
	default:
		return FactualityPass, ""
	}
}

// citationCheck L1 引用存在性检查。
//
// 两级检测：
//  1. 数字声明：content 中的长数字串（≥5位）在 contextDoc 中未出现 → Uncertain（疑似幻觉）
//  2. 关键短语：content 中出现明确引用标记（"according to", "source:", "study shows" 等）
//     但引用内容（紧随引用标记的关键词）未在 contextDoc 中找到 → Fail
func (fg *FactualityGuard) citationCheck(content, contextDoc string) (FactualityVerdict, string) {
	if contextDoc == "" {
		return FactualityUncertain, "no context document for citation check"
	}
	lContent := strings.ToLower(content)
	lContext := strings.ToLower(contextDoc)

	// 检测长数字串（≥5 位）是否在 context 中有出处
	for _, num := range extractNumbers(lContent) {
		if len(num) >= 5 && !strings.Contains(lContext, num) {
			return FactualityUncertain, "specific number '" + num + "' not found in context (possible hallucination)"
		}
	}

	// 检测引用标记后的关键词是否在 context 中
	citationMarkers := []string{
		"according to ", "source: ", "study shows ", "research indicates ",
		"report states ", "data shows ", "survey found ", "statistics show ",
	}
	for _, marker := range citationMarkers {
		idx := strings.Index(lContent, marker)
		if idx < 0 {
			continue
		}
		// 取引用标记之后的前 5 个词作为核验关键词
		after := strings.TrimSpace(lContent[idx+len(marker):])
		words := strings.Fields(after)
		limit := min(5, len(words))
		for _, w := range words[:limit] {
			w = strings.Trim(w, ".,;:\"'")
			if len(w) > 4 && !strings.Contains(lContext, w) {
				return FactualityFail, "cited claim keyword '" + w + "' not found in context document"
			}
		}
	}
	return FactualityPass, ""
}

// L2 NumericalConsistency (numericalCheck/checkProbability/
// checkYearReasonability/checkContextRatio/checkUnitInContext/
// parseSimpleInt) 见 factuality_guard_numeric.go（R7 拆分）。

func (fg *FactualityGuard) emitFail(layer, content, reason string) {
	if fg.OnFail == nil {
		return
	}
	summary := content
	if len(summary) > 100 {
		summary = summary[:100] + "..."
	}
	fg.OnFail(FactualityAlert{
		FailedLayer: layer,
		Content:     summary,
		Reason:      reason,
	})
}

// AddToGate 将 D6 FactualityGuard 注册为 PolicyGate 的 Permit 前置检查钩子。
// 在 gate 评估通过后由调用方在 LLM 输出路径上插入 FactualityGuard.Verify。
// 直接调用 gate.AddForbidRule 添加内容核验失败的阻断规则。
func (fg *FactualityGuard) AddToGate(gate *policy.Gate) error {
	if gate == nil {
		return apperr.New(apperr.CodeInternal, "factuality_guard: gate is nil")
	}
	gate.AddForbidRule(policy.ForbidRule{
		Name:   "factuality_check_failed",
		Reason: "D6 FactualityGuard 核验失败：内容真实性不通过（引用/数值/语义）",
		MatchFn: func(_, action, _ string, ctx map[string]any) bool {
			if action != "emit_response" {
				return false
			}
			failed, _ := ctx["factuality_failed"].(bool)
			return failed
		},
	})
	return nil
}

// truncate 截断字符串到最多 maxRunes 个字符。
func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// extractNumbers 从文本中提取连续数字序列（支持负号前缀，用于引用和数值核验）。
func extractNumbers(text string) []string {
	var nums []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() > 0 {
			s := cur.String()
			// 提取条件：非单独负号，且（长度 >= 3，或有负号且长度 >= 2）
			if s != "-" && (len(s) >= 3 || (len(s) >= 2 && s[0] == '-')) {
				nums = append(nums, s)
			}
			cur.Reset()
		}
	}

	for _, c := range text {
		if c >= '0' && c <= '9' {
			cur.WriteRune(c)
			continue
		}
		if c == '-' && cur.Len() == 0 {
			cur.WriteRune(c)
			continue
		}
		flush()
		if c == '-' {
			cur.WriteRune(c)
		}
	}
	flush()
	return nums
}
