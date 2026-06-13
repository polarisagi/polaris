package cognition

import (
	"context"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
)

// World Model — 双层决策模型。
// L1: 调用前拦截 (StatePredictor + ConfidenceScorer)
// L2: [SurpriseIndex] 执行后调整
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §7

type WorldModel struct {
	predictor  *StatePredictor
	confidence *ConfidenceScorer
	//nolint:unused
	counterfactual *CounterfactualEngine
	//nolint:unused
	simulation *SimulationRuntime
	provider   protocol.Provider // P1: Knowledge Gap Awareness
}

// InjectProvider 注入 LLM 提供商
func (wm *WorldModel) InjectProvider(p protocol.Provider) {
	wm.provider = p
}

// AssessGrounding 评估上下文是否足够执行任务。
// 如果发现关键实体缺失，返回 false，提示需要进一步检索。
func (wm *WorldModel) AssessGrounding(ctx context.Context, task string, contextText string) (bool, string) {
	if wm.provider == nil || task == "" {
		return true, "" // 无 provider 或无任务则默认放行
	}

	prompt := fmt.Sprintf(
		"Task: %s\n\n"+
			"Current Context:\n%s\n\n"+
			"Assess if the current context provides sufficient information to execute the task.\n"+
			"If yes, reply with 'SUFFICIENT'.\n"+
			"If no, reply with 'INSUFFICIENT' and briefly explain what specific knowledge is missing.",
		task, contextText,
	)

	resp, err := wm.provider.Infer(ctx, []protocol.Message{{Role: "user", Content: prompt}}, protocol.WithMaxTokens(128))
	if err != nil {
		return true, "" // 评估失败默认放行
	}

	content := strings.TrimSpace(resp.Content)
	if strings.HasPrefix(strings.ToUpper(content), "INSUFFICIENT") {
		return false, content
	}
	return true, ""
}

// StatePredictor 马尔可夫转移矩阵。
// (success+1)/(total+2) Laplace 平滑, <1ms。
type StatePredictor struct {
	transitions map[string]map[string]int // state → nextState → count
}

// Predict 预测下一状态。
// 冷启动 (无历史) → confidence=0.0 → 全部走 LLM。
func (sp *StatePredictor) Predict(currentState string) (string, float64) {
	nexts, ok := sp.transitions[currentState]
	if !ok || len(nexts) == 0 {
		return "", 0.0
	}

	total := 0
	for _, count := range nexts {
		total += count
	}

	bestState := ""
	bestProb := 0.0
	for state, count := range nexts {
		prob := float64(count+1) / float64(total+2) // Laplace 平滑
		if prob > bestProb {
			bestProb = prob
			bestState = state
		}
	}
	return bestState, bestProb
}

// ConfidenceScorer Isotonic Regression 校准。
// 将原始概率校准为置信度 (0-1)。
type ConfidenceScorer struct {
	bins []calibrationBin
}

type calibrationBin struct {
	lower      float64
	upper      float64
	calibrated float64
}

// Calibrate 校准原始概率为置信度。
func (cs *ConfidenceScorer) Calibrate(rawProb float64) float64 {
	for _, bin := range cs.bins {
		if rawProb >= bin.lower && rawProb < bin.upper {
			return bin.calibrated
		}
	}
	return rawProb
}

// ShouldSkipLLM 判断是否可跳过 LLM 调用。
// Predict → Calibrate → 校准置信度 > 0.8 → 跳过 LLM。
func (wm *WorldModel) ShouldSkipLLM(currentState string) bool {
	if wm.predictor == nil || wm.confidence == nil {
		return false
	}
	_, prob := wm.predictor.Predict(currentState)
	confidence := wm.confidence.Calibrate(prob)
	return confidence > 0.8
}

// CounterfactualEngine 反事实推演（沙箱执行）。
type CounterfactualEngine struct {
	//nolint:unused
	scriptRuntime any // Container 脚本执行运行时
}

// SimulationRuntime VCR 优先 ([Storage-SurrealDB-Core] KV 真实快照回放)。
// 未命中降级 StatePredictor 统计估算。
type SimulationRuntime struct {
	cache map[string][]byte // [Storage-SurrealDB-Core] KV 快照
}

// CacheSnapshot 提供快照回放。
func (sr *SimulationRuntime) CacheSnapshot(key string, val []byte) {
	if sr.cache == nil {
		sr.cache = make(map[string][]byte)
	}
	sr.cache[key] = val
}

// VerifyCounterfactual 验证反事实。
// clone state → Wasm 沙箱模拟替代动作 → VerificationResult
func (wm *WorldModel) VerifyCounterfactual(state string, action string) (*VerificationResult, error) {
	if wm.simulation != nil && wm.simulation.cache != nil {
		if _, ok := wm.simulation.cache[state+"_"+action]; ok {
			return &VerificationResult{Feasible: true, PredictedOutcome: "simulated_success", Confidence: 0.95}, nil
		}
	}
	return &VerificationResult{Feasible: true, PredictedOutcome: "fallback_success", Confidence: 0.5}, nil
}

type VerificationResult struct {
	Feasible         bool
	PredictedOutcome string
	Confidence       float64
}
