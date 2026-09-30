package config

import "testing"

// ADR-0105 决策四：召回预算阈值的默认值与校验。
func TestM4RecallThresholds_DefaultsAndValidate(t *testing.T) {
	d := DefaultThresholds().M4Kernel
	if d.RecallItemMaxChars != 400 || d.RecallMaxTokens != 1200 || d.RecallMinScore != 0 || d.RecallMinScoreRatio != 0.2 {
		t.Fatalf("召回阈值默认值不符：%+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("默认值必须合法：%v", err)
	}

	mutate := map[string]func(*M4KernelThresholds){
		"item_max_chars 为 0":   func(m *M4KernelThresholds) { m.RecallItemMaxChars = 0 },
		"max_tokens 为负":        func(m *M4KernelThresholds) { m.RecallMaxTokens = -1 },
		"min_score 为负":         func(m *M4KernelThresholds) { m.RecallMinScore = -0.1 },
		"min_score_ratio 大于 1": func(m *M4KernelThresholds) { m.RecallMinScoreRatio = 1.5 },
		"min_score_ratio 为负":   func(m *M4KernelThresholds) { m.RecallMinScoreRatio = -0.01 },
	}
	for name, f := range mutate {
		m := d
		f(&m)
		if m.Validate() == nil {
			t.Errorf("%s：必须在加载期报错", name)
		}
	}
	for _, ratio := range []float64{0, 0.5, 1} {
		m := d
		m.RecallMinScoreRatio = ratio
		if err := m.Validate(); err != nil {
			t.Errorf("ratio %v 应合法：%v", ratio, err)
		}
	}
}
