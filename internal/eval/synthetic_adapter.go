package eval

import (
	"github.com/polarisagi/polaris/internal/eval/harness"

	"fmt"

	"github.com/polarisagi/polaris/internal/learning/synthetic"
)

// SyntheticCaseToEvalCase 将 L2 SyntheticCase 转换为评测套件所需的 harness.EvalCase。
//
// 强制上限 P2：自动生成的用例不得触发 P0/P1 阻断逻辑。
// 只有人工标注的 incident 用例（via analysis.IncidentToEvalConverter）才能携带 P0。
//
// 调用方：M9 SelfImprovement 离线批处理写入评测套件时调用；
// 不在 RunSuite 热路径中使用。
func SyntheticCaseToEvalCase(s synthetic.SyntheticCase) harness.EvalCase {
	sev := harness.Severity(s.Severity)
	if sev == "" || sev == harness.SeverityP0 || sev == harness.SeverityP1 {
		sev = harness.SeverityP2
	}
	return harness.EvalCase{
		ID:              s.ID,
		Name:            fmt.Sprintf("Synthetic_%s", s.ID),
		Description:     fmt.Sprintf("Synthetic case from chunk %s (type: %s)", s.ChunkID, s.Type),
		Input:           map[string]any{"question": s.Question},
		Expected:        map[string]any{"ground_truth": s.GroundTruth},
		Level:           harness.Level4LLMJudge,
		Severity:        sev,
		NeedsHumanAudit: s.NeedsHumanAudit,
	}
}
