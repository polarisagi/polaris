package observability

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// Test_SurpriseIndex_ComputeBasic_ColdStart — callCount<3 返回 0.5
func Test_SurpriseIndex_ComputeBasic_ColdStart(t *testing.T) {
	si := NewSurpriseIndex()
	v1 := si.ComputeBasic(context.Background(), []float64{0.1}, []string{"toolA"})
	if v1 != 0.5 {
		t.Errorf("Expected 0.5 on call 1, got %v", v1)
	}
	v2 := si.ComputeBasic(context.Background(), []float64{0.1}, []string{"toolA"})
	if v2 != 0.5 {
		t.Errorf("Expected 0.5 on call 2, got %v", v2)
	}
}

// Test_SurpriseIndex_ComputeBasic_LowSurprise — 相同 embedding/toolSeq 多次后值趋近 0
func Test_SurpriseIndex_ComputeBasic_LowSurprise(t *testing.T) {
	si := NewSurpriseIndex()
	emb := []float64{0.1, 0.2, 0.3}
	tools := []string{"toolA", "toolB"}

	// Warmup
	for i := 0; i < 5; i++ {
		si.ComputeBasic(context.Background(), emb, tools)
	}

	val := si.ComputeBasic(context.Background(), emb, tools)
	if val > 0.1 {
		t.Errorf("Expected low surprise for repeated identical inputs, got %v", val)
	}
}

// Test_SurpriseIndex_ComputeBasic_HighSurprise — 完全不同 embedding/toolSeq 值趋近 1
func Test_SurpriseIndex_ComputeBasic_HighSurprise(t *testing.T) {
	si := NewSurpriseIndex()

	// Base
	emb1 := []float64{1.0, 0.0, 0.0}
	tools1 := []string{"toolA"}
	for i := 0; i < 5; i++ {
		si.ComputeBasic(context.Background(), emb1, tools1)
	}

	// Completely different
	emb2 := []float64{0.0, 1.0, 0.0}
	tools2 := []string{"toolB", "toolC"}

	val := si.ComputeBasic(context.Background(), emb2, tools2)
	if val < 0.6 {
		t.Errorf("Expected high surprise for completely different inputs, got %v", val)
	}
}

// Test_SurpriseIndex_Current_ReflectsLastCompute — Current() == 最近一次 ComputeBasic 结果
func Test_SurpriseIndex_Current_ReflectsLastCompute(t *testing.T) {
	si := NewSurpriseIndex()
	val := si.ComputeBasic(context.Background(), []float64{0.1}, []string{"toolA"})
	if si.Current() != val {
		t.Errorf("Expected Current() %v == lastCompute %v", si.Current(), val)
	}
}

func Test_SurpriseIndex_GatherMetrics(t *testing.T) {
	tbr := NewTokenBurnRate()
	handler := legacyMetricsHandler(tbr)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "polaris_surprise_index ") {
		t.Error("Missing polaris_surprise_index")
	}
	if !strings.Contains(body, "polaris_surprise_index_basic ") {
		t.Error("Missing polaris_surprise_index_basic")
	}
}
