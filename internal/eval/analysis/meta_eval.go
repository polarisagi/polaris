// meta_eval.go — Meta-Eval Sentinel：目标函数完整性审计（V8-S2 缓解机制）。
package analysis

import (
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/harness"

	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// MetaEvalResult Meta-Eval 运行结果。JSON tag 与本仓库其余 HTTP API 的
// snake_case 惯例保持一致（本结构体现经 evaladmin.HandleRunMetaAudit 直接序列化）。
type MetaEvalResult struct {
	MedianFalsifiability float64                      `json:"median_falsifiability"`
	BehaviorTypeCoverage map[harness.BehaviorType]int `json:"behavior_type_coverage"`
	TotalCases           int                          `json:"total_cases"`
	Passed               bool                         `json:"passed"`
	FailureReasons       []string                     `json:"failure_reasons"`
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
// 隔离边界的实际落点是密钥，不是物理进程：meta_auditor 私钥只应存在于运维本地
// （通过 `polaris eval sign` 离线签名），从不写入运行中 server 的配置/环境变量
// （server 侧只持有验签用的公钥 POLARIS_EVAL_PUBKEY_META_AUDITOR）。因此本方法
// 被 evaladmin 包的 HTTP handler 在 server 进程内调用是允许的——服务器进程本身
// 无法伪造一次通过的审计（它没有私钥去产生合法签名），真正被禁止的是让
// AdvanceGate 之类的自动热路径在没有人工触发签名的情况下"自己审计自己"，
// 详见 RunAndRecord 与 internal/prompt/optimizer.MetaAuditReader 的分工说明。
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

// RunAndRecord 运行 Meta-Eval 套件并将结论持久化（store.RecordMetaAuditResult），
// 供 AdvanceGate 之后只读消费。这是唯一应被 HTTP 层（evaladmin）调用的入口——
// 不单独暴露"只跑不落盘"的裸调用给外部触发，避免审计执行与结论持久化出现不一致
// 的中间态（例如跑完了但因为调用方没接着存，AdvanceGate 永远看不到这次结果）。
// 落盘不做独立签名校验：RunMetaEvalSuite 内部读取 meta_holdout 时已验证过
// meta_auditor 签名，身份在同一次调用链中已经确认。
func (m *MetaEvalSentinel) RunAndRecord(ctx context.Context, signature []byte) (*MetaEvalResult, error) {
	result, err := m.RunMetaEvalSuite(ctx, signature)
	if err != nil {
		return nil, err
	}
	if m.store != nil {
		rec := harness.MetaAuditRecord{
			Passed:               result.Passed,
			MedianFalsifiability: result.MedianFalsifiability,
			TotalCases:           result.TotalCases,
			Reasons:              result.FailureReasons,
			ComputedAt:           time.Now().Unix(),
		}
		if recErr := m.store.RecordMetaAuditResult(ctx, rec); recErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "meta_eval: record audit result failed", recErr)
		}
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
