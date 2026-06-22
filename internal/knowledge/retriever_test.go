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

func TestRRFThreeWay(t *testing.T) {
	bm25 := []Chunk{{ID: "c1", Content: "a"}, {ID: "c2", Content: "b"}}
	vec := []Chunk{{ID: "c2", Content: "b"}, {ID: "c3", Content: "c"}}
	graph := []Chunk{{ID: "c1", Content: "a"}}

	out := rrfThreeWay(bm25, vec, graph, 2)
	if len(out) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(out))
	}

	// c2 is highly ranked in vector (weight 0.6) and second in bm25 (0.3)
	// c1 is first in bm25 (0.3) and first in graph (0.1)
	if out[0].ID != "c2" && out[0].ID != "c1" {
		t.Errorf("expected top result to be c1 or c2, got %s", out[0].ID)
	}
}

func TestHybridRetrieverConstructors(t *testing.T) {
	hr := NewHybridRetriever(nil)
	if hr == nil {
		t.Errorf("NewHybridRetriever returned nil")
	}

	hr2 := NewHybridRetrieverWithEmbedder(nil, nil)
	if hr2 == nil {
		t.Errorf("NewHybridRetrieverWithEmbedder returned nil")
	}

	hr3 := NewHybridRetrieverWithCognitive(nil, nil, nil)
	if hr3 == nil {
		t.Errorf("NewHybridRetrieverWithCognitive returned nil")
	}

	hr4 := NewHybridRetrieverWithGraph(nil, nil, nil, nil)
	if hr4 == nil {
		t.Errorf("NewHybridRetrieverWithGraph returned nil")
	}
}

func TestRRFThreeWay_WeightSum(t *testing.T) {
	bm25 := []Chunk{{ID: "a", Content: "a"}, {ID: "b", Content: "b"}}
	vec := []Chunk{{ID: "b", Content: "b"}, {ID: "c", Content: "c"}}
	graph := []Chunk{{ID: "a", Content: "a"}}
	result := rrfThreeWay(bm25, vec, graph, 5)
	if len(result) == 0 {
		t.Fatal("expected non-empty result from rrfThreeWay")
	}
	// "a" 在 BM25 rank=0 + graph rank=0，"b" 在 BM25 rank=1 + vec rank=0
	// "a" 得分: 0.3×(1/61) + 0.1×(1/61) = 0.4/61 ≈ 0.00656
	// "b" 得分: 0.3×(1/62) + 0.6×(1/61) ≈ 0.00484+0.00984 = 0.01467
	// "b" 应高于 "a"（vec 权重高）
	if result[0].ID != "b" {
		t.Errorf("expected 'b' first (high vec weight), got %s", result[0].ID)
	}
}
