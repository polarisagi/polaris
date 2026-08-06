package knowledge

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{1.0, 0.0, 0.0}
	c := []float32{0.0, 1.0, 0.0}

	sim := cosine(a, b)
	if sim < 0.99 {
		t.Errorf("expected ~1.0 for identical vectors, got %f", sim)
	}

	sim = cosine(a, c)
	if sim > 0.01 {
		t.Errorf("expected ~0.0 for orthogonal vectors, got %f", sim)
	}

	sim = cosine([]float32{0, 0}, []float32{0, 0})
	if sim != 0 {
		t.Errorf("expected 0.0 for zero vectors, got %f", sim)
	}
}

func TestParseFloat(t *testing.T) {
	cases := []struct {
		input    string
		expected float64
	}{
		{"0", 0},
		{"1.23", 1.23},
		{"-4.56", -4.56},
		{"1e3", 1000},
		{"-2e-2", -0.02},
		{"+3E+1", 30},
		{"abc", 0},
	}

	for _, c := range cases {
		val := parseFloat(c.input)
		if math.Abs(val-c.expected) > 1e-6 {
			t.Errorf("expected %f for %q, got %f", c.expected, c.input, val)
		}
	}
}

func TestParseEmbedding(t *testing.T) {
	cases := []struct {
		input    string
		expected []float32
		err      bool
	}{
		{"[1.0, 0.5, -2.1]", []float32{1.0, 0.5, -2.1}, false},
		{"[]", nil, false},
		{"not array", nil, true},
		{"[1.0, invalid, 2.0]", []float32{1.0, 0, 2.0}, false}, // simplistic parsing converts "invalid" to 0
	}

	for _, c := range cases {
		res, err := parseEmbedding(c.input)
		if c.err && err == nil {
			t.Errorf("expected error for %q", c.input)
		} else if !c.err && err != nil {
			t.Errorf("unexpected error for %q: %v", c.input, err)
		}

		if len(res) != len(c.expected) {
			t.Errorf("length mismatch for %q: expected %d, got %d", c.input, len(c.expected), len(res))
		}
		for i := range res {
			if math.Abs(float64(res[i]-c.expected[i])) > 1e-5 {
				t.Errorf("mismatch at %d for %q: expected %f, got %f", i, c.input, c.expected[i], res[i])
			}
		}
	}
}

// 2026-07-14（ADR-0062）：NewHybridRetriever/NewHybridRetrieverWithEmbedder/
// NewHybridRetrieverWithGraph 三个构造函数随之删除——boot_knowledge.go 生产唯一
// 使用 NewHybridRetrieverWithCognitive（embedder/cognitive/graph 可传 nil 降级），
// 其余 3 个是从未被启动分级逻辑采纳的平行构造路径。
func TestHybridRetrieverConstructors(t *testing.T) {
	hr3 := NewHybridRetrieverWithCognitive(nil, nil, nil, 0)
	if hr3 == nil {
		t.Errorf("NewHybridRetrieverWithCognitive returned nil")
	}
}
