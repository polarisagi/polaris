package config

import (
	"math"
	"testing"
)

// ADR-0105 决策四/十：召回预算、RRF 权重、相关度门阈值的默认值与校验。
func TestM4RecallThresholds_DefaultsAndValidate(t *testing.T) {
	d := DefaultThresholds().M4Kernel
	if d.RecallItemMaxChars != 400 || d.RecallMaxTokens != 1200 || d.RecallMinScore != 0 || d.RecallMinScoreRatio != 0 {
		t.Fatalf("召回阈值默认值不符：%+v", d)
	}
	if d.RecallWeightReflection != 1.2 || d.RecallWeightEpisodic != 1.1 || d.RecallWeightSemantic != 1.0 || d.RecallWeightRAG != 1.0 {
		t.Fatalf("RRF 来源权重默认值不符：%+v", d)
	}
	if d.RecallWeightReflection <= d.RecallWeightSemantic || d.RecallWeightEpisodic <= d.RecallWeightRAG {
		t.Fatalf("反思/情景权重必须略高于 L2/RAG（决策十）：%+v", d)
	}
	if d.RecallRerankTopN != 12 || d.RecallRerankMinProb != 0.5 || d.RecallRAGMinSurprise != 0.3 {
		t.Fatalf("相关度门/RAG 惊奇门默认值不符：%+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("默认值必须合法：%v", err)
	}

	mutate := map[string]func(*M4KernelThresholds){
		"item_max_chars 为 0":      func(m *M4KernelThresholds) { m.RecallItemMaxChars = 0 },
		"max_tokens 为负":           func(m *M4KernelThresholds) { m.RecallMaxTokens = -1 },
		"min_score 为负":            func(m *M4KernelThresholds) { m.RecallMinScore = -0.1 },
		"min_score_ratio 大于 1":    func(m *M4KernelThresholds) { m.RecallMinScoreRatio = 1.5 },
		"min_score_ratio 为负":      func(m *M4KernelThresholds) { m.RecallMinScoreRatio = -0.01 },
		"weight_rag 为负":           func(m *M4KernelThresholds) { m.RecallWeightRAG = -1 },
		"weight_reflection 为 NaN": func(m *M4KernelThresholds) { m.RecallWeightReflection = math.NaN() },
		"rerank_top_n 为负":         func(m *M4KernelThresholds) { m.RecallRerankTopN = -1 },
		"rerank_min_prob 大于 1":    func(m *M4KernelThresholds) { m.RecallRerankMinProb = 1.01 },
		"rag_min_surprise 为负":     func(m *M4KernelThresholds) { m.RecallRAGMinSurprise = -0.1 },
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

// 权重 0 = 停用来源、TopN 0 = 关闭相关度门：都是合法的"开关"取值。
func TestM4RecallThresholds_ZeroIsSwitch(t *testing.T) {
	m := DefaultThresholds().M4Kernel
	m.RecallWeightRAG, m.RecallRerankTopN = 0, 0
	if err := m.Validate(); err != nil {
		t.Fatalf("权重 0 与 rerank_top_n 0 应合法：%v", err)
	}
}
