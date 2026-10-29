package agentctx

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"
)

// TestContextPressureHint 验证 GD-14-002 上下文压力信号：仅暴露信号供 LLM
// 自主判断，不做 Go 侧强制阈值触发（任务书 8 §8.5 步骤 3 明确要求）。
func TestContextPressureHint(t *testing.T) {
	cases := []struct {
		name        string
		tokenBudget int
		tokensUsed  int
		wantEmpty   bool
		wantSubstr  string
	}{
		{name: "budget unset (0) → no hint", tokenBudget: 0, tokensUsed: 500, wantEmpty: true},
		{name: "low usage → no hint", tokenBudget: 1000, tokensUsed: 100, wantEmpty: true},
		{name: "moderate usage → soft hint", tokenBudget: 1000, tokensUsed: 700, wantSubstr: "moderate"},
		{name: "high usage → strong hint mentioning memory_page_out", tokenBudget: 1000, tokensUsed: 900, wantSubstr: "memory_page_out"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sCtx := &fsm.StateContext{TokenBudget: tc.tokenBudget, TokensUsed: tc.tokensUsed}
			hint := contextPressureHint(sCtx)
			if tc.wantEmpty {
				if hint != "" {
					t.Errorf("expected empty hint, got %q", hint)
				}
				return
			}
			if !strings.Contains(hint, tc.wantSubstr) {
				t.Errorf("expected hint to contain %q, got %q", tc.wantSubstr, hint)
			}
		})
	}
}

// TestContextPressureHint_NilStateContext 防御性验证：nil sCtx 不 panic。
func TestContextPressureHint_NilStateContext(t *testing.T) {
	if hint := contextPressureHint(nil); hint != "" {
		t.Errorf("expected empty hint for nil sCtx, got %q", hint)
	}
}
