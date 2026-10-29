package optimizer

import (
	"context"
	"testing"
)

// GR-7.1-003：外部入口传 nil recent 时，只要有 MemAPO/DB 历史样本，优化管线
// 必须产出候选，而不是在入口处无条件短路。
func TestOptimize_NilRecentUsesStoredSamples(t *testing.T) {
	po := &PromptOptimizer{
		maxBudget: 1000000,
		promptMem: &PromptMemory{entries: map[string][]*PromptStrategy{
			"qa": {{Template: "be concise", SuccessRate: 0.9}},
		}},
	}
	got := po.Optimize(context.Background(), "qa", nil)
	if len(got) == 0 {
		t.Fatal("Optimize with nil recent short-circuited despite stored strategies")
	}
	if len(po.Optimize(context.Background(), "unknown", nil)) != 0 {
		t.Fatal("no samples at all should yield no candidates")
	}
}
