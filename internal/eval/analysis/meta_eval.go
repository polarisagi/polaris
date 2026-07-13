// meta_eval.go — Meta-Eval Sentinel：目标函数完整性审计（V8-S2 缓解机制）。
package analysis

import (
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/harness"

	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// MetaEvalResult Meta-Eval 运行结果。
type MetaEvalResult struct {
	MedianFalsifiability float64
	BehaviorTypeCoverage map[harness.BehaviorType]int
	TotalCases           int
	Passed               bool
	FailureReasons       []string
}

// MetaEvalSentinel Meta-Eval Sentinel 实例（V8-S2 外部锚点，00-Global-Dictionary.md
// §V8-Principle）。审计对象是 EvalHarness 自身的目标函数是否漂移，因此其数据源
// （meta_holdout 分区）必须与被审计的 Training/Validation/Holdout 三层完全隔离，
// 详见 control.PartitionMetaHoldout 注释。
type MetaEvalSentinel struct {
	store                   *harness.SQLiteEvalStore
	FalsifiabilityFloor     float64 // meta_holdout 中位 FalsifiabilityScore 最低值，默认 0.6
	MinBehaviorTypeCoverage int     // 每种 harness.BehaviorType 至少需要的用例数，默认 3
}

// NewMetaEvalSentinel 构造 Sentinel。
func NewMetaEvalSentinel(store *harness.SQLiteEvalStore) *MetaEvalSentinel {
	return &MetaEvalSentinel{
		store:                   store,
		FalsifiabilityFloor:     0.6,
		MinBehaviorTypeCoverage: 3,
	}
}

// RunMetaEvalSuite 运行 Meta-Eval 套件，读取 meta_holdout 分区（非 validation）。
//
// 调用身份固定为 control.RoleMetaAuditor——这不是"审计某个 agentRole"，而是审计
// EvalHarness 目标函数本身是否漂移（V8-S2），signature 由调用方用 meta_auditor
// 私钥签名后传入（开发/测试环境可传 nil，未配置公钥时降级为仅告警）。
//
// 进程边界约束（M12-Eval-Harness.md §5 L2，对 meta_holdout 的适用性强于 Holdout）：
// 本方法不得被塞进运行中 server 进程的热路径（例如
// SQLiteRolloutStore.AdvanceGate）同步调用——那样等于自我审计自己，违反
// V8-Principle"禁止用自动化机制替换外部锚点"。正确用法是作为独立于主进程的
// 人工/CI 审计动作调用，其 Passed/FailureReasons 结论以签名审计记录的形式
// 写回后，供 AdvanceGate 只读消费，而非由 AdvanceGate 直接触发计算。
func (m *MetaEvalSentinel) RunMetaEvalSuite(ctx context.Context, signature []byte) (*MetaEvalResult, error) {
	if m.store == nil {
		return &MetaEvalResult{Passed: false, FailureReasons: []string{"store is nil"}}, nil
	}
	cases, err := m.store.GetMetaHoldoutCases(ctx, control.RoleMetaAuditor, signature)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "meta_eval: load meta_holdout cases failed", err)
	}

	result := &MetaEvalResult{
		BehaviorTypeCoverage: make(map[harness.BehaviorType]int),
		TotalCases:           len(cases),
		Passed:               true,
	}

	if len(cases) == 0 {
		result.Passed = false
		result.FailureReasons = append(result.FailureReasons, "meta_holdout set is empty")
		return result, nil
	}

	var scores []float64
	for _, raw := range cases {
		if c, ok := raw.(harness.EvalCase); ok {
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

	for _, bt := range []harness.BehaviorType{
		harness.BehaviorToolCallSequence, harness.BehaviorSemanticQuality, harness.BehaviorSafetyBoundary,
	} {
		if result.BehaviorTypeCoverage[bt] < m.MinBehaviorTypeCoverage {
			result.Passed = false
			result.FailureReasons = append(result.FailureReasons,
				fmt.Sprintf("behavior_type %q: %d cases (min %d)",
					bt, result.BehaviorTypeCoverage[bt], m.MinBehaviorTypeCoverage))
		}
	}

	if !result.Passed {
		slog.Warn("meta_eval FAILED", "agent_role", control.RoleMetaAuditor,
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
