package swarm

import (
	"math"
	"testing"
)

// ── MarkovMatrix.Update ───────────────────────────────────────────────────────

func TestMarkovMatrix_Update_VocabAndCounts(t *testing.T) {
	m := NewMarkovMatrix()
	m.Update([]string{"bash", "read", "bash"})

	if m.VocabSize() != 2 {
		t.Errorf("expected vocab size 2, got %d", m.VocabSize())
	}
	if m.TotalTransitions() != 2 {
		t.Errorf("expected 2 transitions (bash→read, read→bash), got %f", m.TotalTransitions())
	}
}

func TestMarkovMatrix_Update_ShortSeqNoOp(t *testing.T) {
	m := NewMarkovMatrix()
	m.Update([]string{"only-one"})
	m.Update(nil)
	m.Update([]string{})

	if m.TotalTransitions() != 0 {
		t.Errorf("seq len <2 should add no transitions, got %f", m.TotalTransitions())
	}
}

// ── MarkovMatrix.TransitionProb ───────────────────────────────────────────────

func TestTransitionProb_EmptyMatrix_ReturnsNeutral(t *testing.T) {
	m := NewMarkovMatrix()
	// 无数据时返回中性值 0.5（不影响 Layer A 权重）
	p := m.TransitionProb("bash", "read")
	if p != 0.5 {
		t.Errorf("empty matrix should return 0.5, got %f", p)
	}
}

func TestTransitionProb_AfterUpdate(t *testing.T) {
	m := NewMarkovMatrix()
	// 3 次 bash→read，1 次 bash→write
	m.Update([]string{"bash", "read"})
	m.Update([]string{"bash", "read"})
	m.Update([]string{"bash", "read"})
	m.Update([]string{"bash", "write"})

	// P(read|bash) 应大于 P(write|bash)
	pRead := m.TransitionProb("bash", "read")
	pWrite := m.TransitionProb("bash", "write")
	if pRead <= pWrite {
		t.Errorf("P(read|bash)=%f should be > P(write|bash)=%f", pRead, pWrite)
	}
	// Laplace 平滑：P(read|bash) = (3+1)/(4+|V|) ，|V|={"bash","read","write"}=3
	// = 4/7 ≈ 0.571
	expected := 4.0 / 7.0
	if math.Abs(pRead-expected) > 1e-9 {
		t.Errorf("P(read|bash) expected %f, got %f", expected, pRead)
	}
}

// ── MarkovMatrix.Surprise ─────────────────────────────────────────────────────

func TestSurprise_EmptyMatrix_ReturnsNeutral(t *testing.T) {
	m := NewMarkovMatrix()
	s := m.Surprise([]string{"bash", "read", "write"})
	if s != 0.5 {
		t.Errorf("empty matrix should return 0.5, got %f", s)
	}
}

func TestSurprise_ShortSeq_ReturnsNeutral(t *testing.T) {
	m := NewMarkovMatrix()
	if s := m.Surprise(nil); s != 0.5 {
		t.Errorf("nil seq should return 0.5, got %f", s)
	}
	if s := m.Surprise([]string{"only"}); s != 0.5 {
		t.Errorf("single-element seq should return 0.5, got %f", s)
	}
}

func TestSurprise_FrequentSeqLowerThanRareSeq(t *testing.T) {
	m := NewMarkovMatrix()

	// 训练：bash→read 高频（100次），bash→computer_use 低频（1次）
	for range 100 {
		m.Update([]string{"bash", "read"})
	}
	m.Update([]string{"bash", "computer_use"})

	// 高频序列惊异值应低于低频序列
	surpriseCommon := m.Surprise([]string{"bash", "read"})
	surpriseRare := m.Surprise([]string{"bash", "computer_use"})

	if surpriseCommon >= surpriseRare {
		t.Errorf("common seq surprise=%f should be < rare seq surprise=%f",
			surpriseCommon, surpriseRare)
	}
}

func TestSurprise_InRange(t *testing.T) {
	m := NewMarkovMatrix()
	m.Update([]string{"a", "b", "c", "a", "b"})

	for _, seq := range [][]string{
		{"a", "b", "c"},
		{"a", "a", "a"},
		{"x", "y", "z"}, // 未见转移
	} {
		s := m.Surprise(seq)
		if s < 0 || s > 1 {
			t.Errorf("surprise %f for seq %v out of [0,1]", s, seq)
		}
	}
}

// ── SurpriseCalculator：构造与阈值 ────────────────────────────────────────────

func TestNewSurpriseCalculator_DefaultThreshold(t *testing.T) {
	calc := NewSurpriseCalculator(nil)
	if calc.layerBThreshold != DefaultLayerBThreshold {
		t.Errorf("expected default threshold %f, got %f", DefaultLayerBThreshold, calc.layerBThreshold)
	}
	if calc.markov == nil {
		t.Error("markov should be initialized (not nil) at construction")
	}
}

func TestNewSurpriseCalculatorWith_CustomThreshold(t *testing.T) {
	calc := NewSurpriseCalculatorWith(nil, 500)
	if calc.layerBThreshold != 500 {
		t.Errorf("expected threshold 500, got %f", calc.layerBThreshold)
	}
}

func TestNewSurpriseCalculatorWith_BelowMinClamped(t *testing.T) {
	for _, input := range []float64{0, 1, 100, 499} {
		calc := NewSurpriseCalculatorWith(nil, input)
		if calc.layerBThreshold != MinLayerBThreshold {
			t.Errorf("input %f: expected clamp to %f, got %f",
				input, MinLayerBThreshold, calc.layerBThreshold)
		}
	}
}

// ── SurpriseCalculator：自动激活 Layer B ──────────────────────────────────────

func TestSurpriseCalculator_AutoActivation_BelowThreshold(t *testing.T) {
	// 低阈值让少量数据即可触发 Layer B，验证自动切换无 panic
	calc := NewSurpriseCalculatorWith(nil, 2)

	// 先积累超过阈值的数据
	prep := &CalcRequest{
		TaskID:   "prep",
		TaskType: "code",
		ToolSeq:  []string{"bash", "read", "bash"},
		ResultCh: make(chan float64, 1),
	}
	if !calc.Submit(prep) {
		t.Fatal("submit should succeed")
	}
	<-prep.ResultCh

	// 此时 markov 内已有 2 次转移，再提交一个请求验证 Layer B 路径
	req := &CalcRequest{
		TaskID:   "t1",
		TaskType: "code",
		ToolSeq:  []string{"bash", "read"},
		ResultCh: make(chan float64, 1),
	}
	if !calc.Submit(req) {
		t.Fatal("submit should succeed")
	}
	result := <-req.ResultCh
	if result < 0 || result > 1 {
		t.Errorf("Layer B result %f out of [0,1]", result)
	}
}

func TestSurpriseCalculator_Baseline_FallbackBelowThreshold(t *testing.T) {
	calc := NewSurpriseCalculator(nil) // threshold=1000，新矩阵无数据 → Tier-0 基线
	req := &CalcRequest{
		TaskID:   "t1",
		TaskType: "code",
		ToolSeq:  []string{"bash", "computer_use"},
		ResultCh: make(chan float64, 1),
	}
	if !calc.Submit(req) {
		t.Fatal("submit should succeed")
	}
	result := <-req.ResultCh
	if result < 0 || result > 1 {
		t.Errorf("result %f out of [0,1]", result)
	}
}

func TestSurpriseCalculator_LayerB_NotActivatedBeforeThreshold(t *testing.T) {
	calc := NewSurpriseCalculator(nil)
	m := NewMarkovMatrix()
	// 积累不足 1000 次转移 → Tier-0 基线
	m.Update([]string{"bash", "read"})
	calc.WithMarkovMatrix(m)

	req := &CalcRequest{
		TaskID:   "t2",
		TaskType: "code",
		ToolSeq:  []string{"bash", "read"},
		ResultCh: make(chan float64, 1),
	}
	if !calc.Submit(req) {
		t.Fatal("submit should succeed")
	}
	<-req.ResultCh // 消费结果，主要验证无 panic
}

// ── WithMarkovMatrix：warm-start 替换 ─────────────────────────────────────────

func TestWithMarkovMatrix_ReplacesInternal(t *testing.T) {
	calc := NewSurpriseCalculator(nil)
	original := calc.markov

	m := NewMarkovMatrix()
	m.Update([]string{"bash", "read"})
	calc.WithMarkovMatrix(m)

	calc.mu.Lock()
	replaced := calc.markov
	calc.mu.Unlock()

	if replaced == original {
		t.Error("WithMarkovMatrix should replace the internal matrix")
	}
	if replaced.TotalTransitions() != 1 {
		t.Errorf("expected 1 transition in replaced matrix, got %f", replaced.TotalTransitions())
	}
}
