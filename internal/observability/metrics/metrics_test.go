package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// Test_SurpriseIndex_ComputeBasic_ColdStart — [Task 13] 冷启动阈值 callCount<10
// 返回固定 0.5。2026-07-04 审计修复：原注释/断言仍停留在升级前的 callCount<3，
// 与实际生效的阈值（metrics.go ComputeBasic 的 `if si.callCount < 10`）不符，
// 且只验证了前 2 次调用，从未验证过阈值边界（第 9/10 次）。
func Test_SurpriseIndex_ComputeBasic_ColdStart(t *testing.T) {
	si := NewSurpriseIndex()
	for i := 1; i <= 9; i++ {
		v := si.ComputeBasic(context.Background(), []float64{0.1}, []string{"toolA"})
		if v != 0.5 {
			t.Errorf("Expected 0.5 on call %d (callCount<10), got %v", i, v)
		}
	}
	// 第 10 次调用起 callCount 不再 <10，应开始产生真实计算值（不再恒为 0.5）。
	v10 := si.ComputeBasic(context.Background(), []float64{0.9}, []string{"toolZ"})
	if v10 == 0.5 {
		t.Error("Expected non-fixed value once callCount reaches 10, still got 0.5")
	}
}

// Test_SurpriseIndex_ComputeBasic_OrderSensitive — [Task 13] 核心修复点回归测试：
// 相同内容、不同顺序的工具调用序列必须产生不同的 divergence。Jaccard 集合相似度
// 会让 [A,B,C] 与 [C,B,A] 判定为完全相同（丢失顺序信息），Levenshtein 编辑距离
// 应能捕捉这一差异。2026-07-04 审计修复：此前 task 13 验收标准明确要求此项测试
// 但从未补充。
func Test_SurpriseIndex_ComputeBasic_OrderSensitive(t *testing.T) {
	emb := []float64{0.1, 0.2, 0.3}

	// 场景 A：序列顺序不变（[A,B,C] -> [A,B,C]）
	siSame := NewSurpriseIndex()
	for i := 0; i < 10; i++ {
		siSame.ComputeBasic(context.Background(), emb, []string{"toolA", "toolB", "toolC"})
	}
	sameOrderVal := siSame.ComputeBasic(context.Background(), emb, []string{"toolA", "toolB", "toolC"})

	// 场景 B：内容相同、顺序颠倒（[A,B,C] -> [C,B,A]）
	siReversed := NewSurpriseIndex()
	for i := 0; i < 10; i++ {
		siReversed.ComputeBasic(context.Background(), emb, []string{"toolA", "toolB", "toolC"})
	}
	reversedOrderVal := siReversed.ComputeBasic(context.Background(), emb, []string{"toolC", "toolB", "toolA"})

	if reversedOrderVal <= sameOrderVal {
		t.Errorf("expected reversed-order sequence to produce higher surprise than same-order repeat (Levenshtein should be order-sensitive), same=%v reversed=%v", sameOrderVal, reversedOrderVal)
	}
	// 差异应显著（不是浮点误差级别），否则说明退化回了 Jaccard 时代的集合比较。
	if reversedOrderVal-sameOrderVal < 0.05 {
		t.Errorf("expected meaningfully higher surprise for reversed order (looks like set-based comparison, not sequence-aware), same=%v reversed=%v", sameOrderVal, reversedOrderVal)
	}
}

// Test_SurpriseIndex_ComputeBasic_LowSurprise — 相同 embedding/toolSeq 多次后值趋近 0
func Test_SurpriseIndex_ComputeBasic_LowSurprise(t *testing.T) {
	si := NewSurpriseIndex()
	emb := []float64{0.1, 0.2, 0.3}
	tools := []string{"toolA", "toolB"}

	// Warmup
	for i := 0; i < 15; i++ {
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
	for i := 0; i < 15; i++ {
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
