package sleeptime

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// VerifyProposals 在 held-in (训练集) 与 held-out (测试集) 双切分回归集上验证改动效果。
// 规则（ADR-0115 WP6 / Self-Harness）：至少一侧提升且另一侧不退化才接受。
// 即：candIn >= baseIn && candOut >= baseOut && (candIn > baseIn || candOut > baseOut)。
func VerifyProposals(
	ctx context.Context,
	evaluator Evaluator,
	changes []ProposedChange,
) (accepted bool, baseIn, baseOut, candIn, candOut float64, reason string, err error) {
	if evaluator == nil {
		return false, 0, 0, 0, 0, "evaluator not configured", apperr.New(apperr.CodeInternal, "sleeptime.VerifyProposals: nil evaluator")
	}

	// 1. 基线评分
	baseIn, baseOut, err = evaluator.Evaluate(ctx, nil)
	if err != nil {
		return false, 0, 0, 0, 0, "baseline evaluation failed", apperr.Wrap(apperr.CodeInternal, "sleeptime: baseline eval failed", err)
	}

	// 2. 候选变动评分
	files := make(map[string][]byte, len(changes))
	for _, c := range changes {
		files[c.Path] = []byte(c.Content)
	}

	candIn, candOut, err = evaluator.Evaluate(ctx, files)
	if err != nil {
		return false, baseIn, baseOut, 0, 0, "candidate evaluation failed", apperr.Wrap(apperr.CodeInternal, "sleeptime: candidate eval failed", err)
	}

	// 3. 裁决逻辑
	if candIn < baseIn {
		reason = fmt.Sprintf("held-in regression: candidate %.3f < baseline %.3f", candIn, baseIn)
		return false, baseIn, baseOut, candIn, candOut, reason, nil
	}
	if candOut < baseOut {
		reason = fmt.Sprintf("held-out regression: candidate %.3f < baseline %.3f", candOut, baseOut)
		return false, baseIn, baseOut, candIn, candOut, reason, nil
	}
	if candIn == baseIn && candOut == baseOut {
		reason = fmt.Sprintf("no improvement: held-in=%.3f, held-out=%.3f", candIn, candOut)
		return false, baseIn, baseOut, candIn, candOut, reason, nil
	}

	reason = fmt.Sprintf("accepted: held-in %.3f->%.3f, held-out %.3f->%.3f", baseIn, candIn, baseOut, candOut)
	return true, baseIn, baseOut, candIn, candOut, reason, nil
}
