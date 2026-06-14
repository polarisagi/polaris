// meta_eval.go — Meta-Eval Sentinel：目标函数完整性审计（V8-S2 缓解机制）。
package eval

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
)

// MetaEvalResult Meta-Eval 运行结果。
type MetaEvalResult struct {
	MedianFalsifiability float64
	BehaviorTypeCoverage map[BehaviorType]int
	TotalCases           int
	Passed               bool
	FailureReasons       []string
}

// MetaEvalSentinel Meta-Eval Sentinel 实例。
type MetaEvalSentinel struct {
	store                   *SQLiteEvalStore
	FalsifiabilityFloor     float64 // Holdout 中位 FalsifiabilityScore 最低值，默认 0.6
	MinBehaviorTypeCoverage int     // 每种 BehaviorType 至少需要的用例数，默认 3
}

// NewMetaEvalSentinel 构造 Sentinel。
func NewMetaEvalSentinel(store *SQLiteEvalStore) *MetaEvalSentinel {
	return &MetaEvalSentinel{
		store:                   store,
		FalsifiabilityFloor:     0.6,
		MinBehaviorTypeCoverage: 3,
	}
}

// RunMetaEvalSuite 运行 Meta-Eval 套件。
// agentRole: 要审计的 Holdout partition 的 agentRole（通常 "default"）。
// 建议在 M9 L3/L4 Rollout 前调用（rollout.go 触发点）。
func (m *MetaEvalSentinel) RunMetaEvalSuite(ctx context.Context, agentRole string) (*MetaEvalResult, error) {
	if m.store == nil {
		return &MetaEvalResult{Passed: false, FailureReasons: []string{"store is nil"}}, nil
	}
	cases, err := m.store.GetValidationCases(ctx, agentRole, nil) // nil sig = 开发模式
	if err != nil {
		return nil, fmt.Errorf("meta_eval: load validation cases: %w", err)
	}

	result := &MetaEvalResult{
		BehaviorTypeCoverage: make(map[BehaviorType]int),
		TotalCases:           len(cases),
		Passed:               true,
	}

	if len(cases) == 0 {
		result.Passed = false
		result.FailureReasons = append(result.FailureReasons, "holdout set is empty")
		return result, nil
	}

	var scores []float64
	for _, raw := range cases {
		if c, ok := raw.(EvalCase); ok {
			scores = append(scores, c.FalsifiabilityScore)
			result.BehaviorTypeCoverage[c.BehaviorType]++
		}
	}
	result.MedianFalsifiability = medianF64(scores)

	if result.MedianFalsifiability < m.FalsifiabilityFloor {
		result.Passed = false
		result.FailureReasons = append(result.FailureReasons,
			fmt.Sprintf("median falsifiability %.2f < floor %.2f",
				result.MedianFalsifiability, m.FalsifiabilityFloor))
	}

	for _, bt := range []BehaviorType{
		BehaviorToolCallSequence, BehaviorSemanticQuality, BehaviorSafetyBoundary,
	} {
		if result.BehaviorTypeCoverage[bt] < m.MinBehaviorTypeCoverage {
			result.Passed = false
			result.FailureReasons = append(result.FailureReasons,
				fmt.Sprintf("behavior_type %q: %d cases (min %d)",
					bt, result.BehaviorTypeCoverage[bt], m.MinBehaviorTypeCoverage))
		}
	}

	if !result.Passed {
		slog.Warn("meta_eval FAILED", "agent_role", agentRole,
			"median_falsifiability", result.MedianFalsifiability,
			"reasons", result.FailureReasons)
	}
	return result, nil
}

func medianF64(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	s := make([]float64, len(data))
	copy(s, data)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 0 {
		return (s[mid-1] + s[mid]) / 2
	}
	return s[mid]
}
