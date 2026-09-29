package action

import (
	"strings"
	"testing"
)

func TestNewPolicyEvolver(t *testing.T) {
	e := NewPolicyEvolver(0, 0)
	if e.window != 50 {
		t.Errorf("Expected default window 50, got %v", e.window)
	}
	if e.minRate != 0.6 {
		t.Errorf("Expected default minRate 0.6, got %v", e.minRate)
	}

	e2 := NewPolicyEvolver(10, 0.8)
	if e2.window != 10 {
		t.Errorf("Expected window 10, got %v", e2.window)
	}
	if e2.minRate != 0.8 {
		t.Errorf("Expected minRate 0.8, got %v", e2.minRate)
	}
}

func TestPolicyEvolver_RegisterAndGetPolicy(t *testing.T) {
	e := NewPolicyEvolver(10, 0.6)

	policy := &ToolUsagePolicy{
		ToolName: "test_tool",
		BestFor:  []string{"testing"},
	}

	e.RegisterPolicy(policy)

	p := e.GetPolicy("test_tool")
	if p == nil || p.ToolName != "test_tool" {
		t.Errorf("Failed to get registered policy")
	}

	policies := e.ListPolicies()
	if len(policies) != 1 {
		t.Errorf("Expected 1 policy in list, got %v", len(policies))
	}
}

func TestPolicyEvolver_SuccessRate(t *testing.T) {
	e := NewPolicyEvolver(5, 0.6)

	if rate := e.SuccessRate("test_tool"); rate != -1 {
		t.Errorf("Expected -1 for no history, got %v", rate)
	}

	e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: true})
	e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: false})

	if rate := e.SuccessRate("test_tool"); rate != 0.5 {
		t.Errorf("Expected 0.5 success rate, got %v", rate)
	}
}

func TestPolicyEvolver_Evolve_LowSuccessRate(t *testing.T) {
	e := NewPolicyEvolver(5, 0.6)

	// Add 5 outcomes, 1 success, 4 failures -> 20% success rate
	for i := 0; i < 4; i++ {
		e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: false, Error: "error"})
	}
	e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: true})

	policy := e.GetPolicy("test_tool")
	if policy == nil {
		t.Fatal("Policy should be auto-created")
	}

	if !containsStr(policy.NotRecommendedFor, "high_failure_rate") {
		t.Error("Expected high_failure_rate in NotRecommendedFor")
	}

	// Add 5 successes to push success rate to 100% (window is 5)
	for i := 0; i < 5; i++ {
		e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: true})
	}

	if containsStr(policy.NotRecommendedFor, "high_failure_rate") {
		t.Error("Expected high_failure_rate to be removed")
	}
}

func TestPolicyEvolver_Evolve_HighLatency(t *testing.T) {
	e := NewPolicyEvolver(5, 0.6)

	// Add 5 outcomes with > 5000 latency
	for i := 0; i < 5; i++ {
		e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: true, LatencyMs: 6000})
	}

	policy := e.GetPolicy("test_tool")
	if policy == nil {
		t.Fatal("Policy should be auto-created")
	}

	if hint, ok := policy.ParamHints["timeout_ms"]; !ok || hint.DefaultValue.(int64) != 12000 {
		t.Errorf("Expected timeout_ms hint with 12000, got %v", hint)
	}
}

func TestPolicyEvolver_FailurePattern(t *testing.T) {
	e := NewPolicyEvolver(10, 0.6)

	// First 4 failures do not trigger evolve
	for i := 0; i < 4; i++ {
		e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: false, Error: "TimeoutError"})
	}

	// Next 3 failures trigger evolve and increment frequency
	for i := 0; i < 3; i++ {
		e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: false, Error: "TimeoutError"})
	}

	pattern := e.patterns["test_tool"]["TimeoutError"]
	if pattern == nil {
		t.Fatal("Expected FailurePattern to be recorded")
	}
	if pattern.Frequency != 3 {
		t.Errorf("Expected frequency 3, got %v", pattern.Frequency)
	}
	if pattern.Mitigation == "" {
		t.Errorf("Expected mitigation to be auto-generated")
	}
}

func TestPolicyEvolver_Hints(t *testing.T) {
	e := NewPolicyEvolver(25, 0.6)

	// Less than 20 history
	e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: true})
	if hint := e.GetContextHint("test_tool"); hint != "" {
		t.Errorf("Expected empty hint for < 20 history, got %v", hint)
	}
	if block := e.BuildSystemHintBlock(); block != "" {
		t.Errorf("Expected empty system hint block for < 20 history, got %v", block)
	}

	// 20+ history
	for i := 0; i < 20; i++ {
		e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: false, Error: "CommonError"})
	}

	policy := e.GetPolicy("test_tool")
	policy.ParamHints["param1"] = ParamHint{DefaultValue: "val"}

	hint := e.GetContextHint("test_tool")
	if !strings.Contains(hint, "ParamHints: param1") {
		t.Errorf("Expected ParamHints in hint, got %v", hint)
	}
	if !strings.Contains(hint, "FailureWarning: Frequent error 'CommonError'") {
		t.Errorf("Expected FailureWarning in hint, got %v", hint)
	}

	block := e.BuildSystemHintBlock()
	if !strings.Contains(block, "<tool-hints>") || !strings.Contains(block, "test_tool") {
		t.Errorf("Expected valid tool-hints block, got %v", block)
	}

	// Unregistered tool
	if hint := e.GetContextHint("unknown_tool"); hint != "" {
		t.Errorf("Expected empty hint for unknown tool")
	}
}

func TestPolicyEvolver_BuildSystemHintBlock_NoHints(t *testing.T) {
	e := NewPolicyEvolver(25, 0.6)
	for i := 0; i < 20; i++ {
		e.RecordOutcome(ToolOutcome{ToolName: "test_tool", Success: true})
	}

	// Policy has no hints, no failure patterns
	if block := e.BuildSystemHintBlock(); block != "" {
		t.Errorf("Expected empty system hint block when no hints exist, got %v", block)
	}
}

func TestContainsStrAndRemoveStr(t *testing.T) {
	ss := []string{"a", "b", "c"}
	if !containsStr(ss, "b") {
		t.Error("Expected to contain 'b'")
	}
	if containsStr(ss, "d") {
		t.Error("Did not expect to contain 'd'")
	}

	ss = removeStr(ss, "b")
	if containsStr(ss, "b") {
		t.Error("Expected 'b' to be removed")
	}
	if len(ss) != 2 {
		t.Errorf("Expected length 2, got %v", len(ss))
	}
}

// WarmStart 重放后的窗口/成功率/策略标注应与逐条实时 RecordOutcome 完全一致，
// 且窗口超限时只保留最近 window 条。
func TestPolicyEvolver_WarmStart_MatchesLive(t *testing.T) {
	outcomes := make([]ToolOutcome, 0, 12)
	for i := range 12 {
		outcomes = append(outcomes, ToolOutcome{ToolName: "bash", Success: i%3 != 0, LatencyMs: 10, Error: "boom"})
	}
	live := NewPolicyEvolver(10, 0.9)
	for _, o := range outcomes {
		live.RecordOutcome(o)
	}
	warm := NewPolicyEvolver(10, 0.9)
	warm.WarmStart(outcomes)

	if got, want := warm.SuccessRate("bash"), live.SuccessRate("bash"); got != want {
		t.Fatalf("SuccessRate warm=%f live=%f", got, want)
	}
	// 12 条里 i=0,3,6,9 失败；窗口 10 条 = i=2..11，失败 3,6,9 共 3 条 → 成功率 0.7。
	if got := warm.SuccessRate("bash"); got < 0.699 || got > 0.701 {
		t.Fatalf("窗口应只留最近 10 条，成功率 0.7，got %f", got)
	}
	p := warm.GetPolicy("bash")
	if p == nil || !containsStr(p.NotRecommendedFor, "high_failure_rate") {
		t.Fatalf("成功率 0.7 < 0.9 应标注 high_failure_rate: %+v", p)
	}
	if warm.Window() != 10 {
		t.Fatalf("Window()=%d", warm.Window())
	}
}

func TestPolicyEvolver_WarmStart_EmptyAndThenLive(t *testing.T) {
	e := NewPolicyEvolver(0, 0)
	e.WarmStart(nil)
	if e.SuccessRate("x") != -1 {
		t.Fatal("空 warm-start 不应产生历史")
	}
	e.WarmStart([]ToolOutcome{{ToolName: "x", Success: true}, {ToolName: "x", Success: false}})
	e.RecordOutcome(ToolOutcome{ToolName: "x", Success: true})
	if got := e.SuccessRate("x"); got < 0.666 || got > 0.667 {
		t.Fatalf("warm-start 之后继续实时累积，want 2/3 got %f", got)
	}
}
