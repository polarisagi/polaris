package substrate

import (
	"math"
	"testing"
)

// TestCorpusStats_AvgDocLen_ColdStart — 无文档时返回 100.0
func TestCorpusStats_AvgDocLen_ColdStart(t *testing.T) {
	cs := NewCorpusStats()
	if cs.AvgDocLen() != 100.0 {
		t.Errorf("Expected 100.0, got %v", cs.AvgDocLen())
	}
}

// TestCorpusStats_IDF_UnseenTerm — 未见词返回 1.5
func TestCorpusStats_IDF_UnseenTerm(t *testing.T) {
	cs := NewCorpusStats()
	if cs.IDF("hello") != 1.5 {
		t.Errorf("Expected 1.5, got %v", cs.IDF("hello"))
	}
}

// TestCorpusStats_IDF_RobertsonFormula — 已知 N、df 计算结果精确匹配
func TestCorpusStats_IDF_RobertsonFormula(t *testing.T) {
	cs := NewCorpusStats()
	cs.docCount = 10
	cs.termDocFreq["test"] = 3

	// Robertson IDF: log((N - df + 0.5) / (df + 0.5) + 1)
	expected := math.Log(float64(10-3+1)/float64(3+1) + 1.0)
	if cs.IDF("test") != expected {
		t.Errorf("Expected %v, got %v", expected, cs.IDF("test"))
	}
}

// TestBM25Score_WithStats_BetterThanFixed — 有语料统计时评分差异度比固定值更大
func TestBM25Score_WithStats_BetterThanFixed(t *testing.T) {
	cs := NewCorpusStats()
	cs.AddDoc([]string{"hello", "world"})
	cs.AddDoc([]string{"world", "test"})
	cs.AddDoc([]string{"world", "unique"})

	// "world" is common, "unique" is rare
	scoreFixedWorld := bm25Score("hello world", "world", nil)
	scoreFixedUnique := bm25Score("hello unique", "unique", nil)

	scoreStatsWorld := bm25Score("hello world", "world", cs)
	scoreStatsUnique := bm25Score("hello unique", "unique", cs)

	// With stats, "unique" should get a much higher boost compared to "world"
	// Without stats, they get the exact same score if the document length is the same.
	diffFixed := math.Abs(scoreFixedUnique - scoreFixedWorld)
	diffStats := math.Abs(scoreStatsUnique - scoreStatsWorld)

	if diffStats <= diffFixed {
		t.Errorf("Expected stats to provide more differentiation between rare and common terms")
	}
}
