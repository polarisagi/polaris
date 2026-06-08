package governance

import (
	"context"
	"strings"
	"testing"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

func TestL1AssertionEvaluator(t *testing.T) {
	eval := &L1AssertionEvaluator{
		assertions: []Assertion{
			{Type: "contains", Name: "MustContainSuccess", Value: "SUCCESS"},
			{Type: "not_contains", Name: "MustNotContainError", Value: "ERROR"},
			{Type: "regex", Name: "MatchCode", Value: `[0-9]+`},
			{Type: "length_under", Name: "ShortOutput", Value: "100"},
			{Type: "tool_called", Name: "UsedShell", Value: "shell"},
			{Type: "no_tool_called", Name: "NoRm", Value: "rm"},
			{Type: "cost_under", Name: "Cheap", Value: "0.10"},
			{Type: "steps_under", Name: "Fast", Value: "5"},
		},
	}

	traj := &AgentTrajectory{
		Steps: make([]TrajectoryEvent, 3), // length = 3
		Result: &TrajectoryResult{
			Output:    "Execution finished with status SUCCESS 42.",
			ToolCalls: []string{"ls", "shell"},
			CostUSD:   0.05,
		},
	}

	res, err := eval.Evaluate(traj, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Passed {
		t.Errorf("expected passing trajectory, failed at: %s", res.Details)
	}

	// 触发失败的用例
	badTraj := &AgentTrajectory{
		Result: &TrajectoryResult{
			Output:    "ERROR 42",
			ToolCalls: []string{"rm"},
		},
	}

	res2, _ := eval.Evaluate(badTraj, nil)
	if res2.Passed {
		t.Errorf("expected failure for bad trajectory")
	}
}

// 简单的 Mock Provider 用于测试 L4 Judge
type mockJudgeProvider struct {
	response string
}

func (m *mockJudgeProvider) Infer(ctx context.Context, req *protocol.InferRequest) (*protocol.InferResponse, error) {
	return &protocol.InferResponse{Content: m.response}, nil
}
func (m *mockJudgeProvider) StreamInfer(ctx context.Context, req *protocol.InferRequest) (<-chan protocol.StreamEvent, error) {
	return nil, nil
}
func (m *mockJudgeProvider) Capabilities() protocol.ProviderCapabilities {
	return protocol.ProviderCapabilities{}
}
func (m *mockJudgeProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockJudgeProvider) ModelID() string                      { return "mock" }

func TestL4LLMJudgeEvaluator(t *testing.T) {
	eval := &L4LLMJudgeEvaluator{
		provider: &mockJudgeProvider{
			response: `{"scores": {"Safety": 5.0, "Efficiency": 4.0}, "reasoning": "Looks good."}`,
		},
		rubric: []RubricDimension{
			{Name: "Safety", Weight: 1.0},
			{Name: "Efficiency", Weight: 0.5},
		},
		passThreshold: 4.0,
	}

	traj := &AgentTrajectory{
		Result: &TrajectoryResult{Output: "Done"},
	}
	expected := &EvalCase{ExpectedOutput: "Done"}

	res, err := eval.Evaluate(traj, expected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Passed {
		t.Errorf("expected to pass, details: %s", res.Details)
	}

	// 计算权重应为: (5*1 + 4*0.5) / 1.5 = 7 / 1.5 = 4.66
	if res.Scores["Safety"] != 5.0 {
		t.Errorf("missing parsed score for Safety")
	}
}

// ─── L2SchemaEvaluator ────────────────────────────────────────────────────────

func TestL2SchemaEvaluator_Pass_NonJSON(t *testing.T) {
	e := &L2SchemaEvaluator{}
	traj := &AgentTrajectory{Result: &TrajectoryResult{Output: "plain text"}}
	ec := &EvalCase{ExpectedOutput: "plain text"}
	res, err := e.Evaluate(traj, ec)
	if err != nil || !res.Passed {
		t.Fatalf("expected pass for non-JSON expected output")
	}
}

func TestL2SchemaEvaluator_Pass_JSONOutput(t *testing.T) {
	e := &L2SchemaEvaluator{}
	traj := &AgentTrajectory{Result: &TrajectoryResult{Output: `{"status":"ok"}`}}
	ec := &EvalCase{ExpectedOutput: `{"status":"ok"}`}
	res, err := e.Evaluate(traj, ec)
	if err != nil || !res.Passed {
		t.Fatalf("expected pass for valid JSON output, got: %v %v", err, res)
	}
}

func TestL2SchemaEvaluator_Fail_InvalidJSON(t *testing.T) {
	e := &L2SchemaEvaluator{}
	traj := &AgentTrajectory{Result: &TrajectoryResult{Output: "not json"}}
	ec := &EvalCase{ExpectedOutput: `{"key":"value"}`}
	res, err := e.Evaluate(traj, ec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Passed {
		t.Fatal("expected fail for invalid JSON output")
	}
}

func TestL2SchemaEvaluator_NilInputs(t *testing.T) {
	e := &L2SchemaEvaluator{}
	res, err := e.Evaluate(nil, nil)
	if err != nil || !res.Passed {
		t.Fatal("expected pass for nil inputs")
	}
}

// ─── L3TrajectoryEvaluator ────────────────────────────────────────────────────

func TestL3Exact_Pass(t *testing.T) {
	e := &L3TrajectoryEvaluator{mode: "exact"}
	traj := &AgentTrajectory{Result: &TrajectoryResult{ToolCalls: []string{"search", "read"}}}
	ec := &EvalCase{ExpectedToolCalls: []ExpectedToolCall{
		{ToolName: "search"}, {ToolName: "read"},
	}}
	res, err := e.Evaluate(traj, ec)
	if err != nil || !res.Passed {
		t.Fatalf("expected pass for exact match, got: %v %v", err, res)
	}
}

func TestL3Exact_Fail_DifferentOrder(t *testing.T) {
	e := &L3TrajectoryEvaluator{mode: "exact"}
	traj := &AgentTrajectory{Result: &TrajectoryResult{ToolCalls: []string{"read", "search"}}}
	ec := &EvalCase{ExpectedToolCalls: []ExpectedToolCall{
		{ToolName: "search"}, {ToolName: "read"},
	}}
	res, _ := e.Evaluate(traj, ec)
	if res.Passed {
		t.Fatal("expected fail for wrong order")
	}
}

func TestL3Subset_Pass(t *testing.T) {
	e := &L3TrajectoryEvaluator{mode: "subset"}
	traj := &AgentTrajectory{Result: &TrajectoryResult{ToolCalls: []string{"read"}}}
	ec := &EvalCase{ExpectedToolCalls: []ExpectedToolCall{
		{ToolName: "search"}, {ToolName: "read"},
	}}
	res, _ := e.Evaluate(traj, ec)
	if !res.Passed {
		t.Fatal("expected pass: actual is subset of expected")
	}
}

func TestL3Subset_Fail(t *testing.T) {
	e := &L3TrajectoryEvaluator{mode: "subset"}
	traj := &AgentTrajectory{Result: &TrajectoryResult{ToolCalls: []string{"write"}}}
	ec := &EvalCase{ExpectedToolCalls: []ExpectedToolCall{{ToolName: "search"}}}
	res, _ := e.Evaluate(traj, ec)
	if res.Passed {
		t.Fatal("expected fail: write not in expected set")
	}
}

func TestL3Contains_Pass(t *testing.T) {
	e := &L3TrajectoryEvaluator{mode: "contains"}
	traj := &AgentTrajectory{Result: &TrajectoryResult{ToolCalls: []string{"search", "read", "write"}}}
	ec := &EvalCase{ExpectedToolCalls: []ExpectedToolCall{
		{ToolName: "search"}, {ToolName: "read"},
	}}
	res, _ := e.Evaluate(traj, ec)
	if !res.Passed {
		t.Fatal("expected pass: actual contains all expected")
	}
}

func TestL3Contains_Fail_MissingExpected(t *testing.T) {
	e := &L3TrajectoryEvaluator{mode: "contains"}
	traj := &AgentTrajectory{Result: &TrajectoryResult{ToolCalls: []string{"search"}}}
	ec := &EvalCase{ExpectedToolCalls: []ExpectedToolCall{
		{ToolName: "search"}, {ToolName: "read"},
	}}
	res, _ := e.Evaluate(traj, ec)
	if res.Passed {
		t.Fatal("expected fail: actual missing 'read'")
	}
	if !strings.Contains(res.Details, "read") {
		t.Errorf("details should mention missing tool, got: %s", res.Details)
	}
}

func TestL3_NilTrajectory(t *testing.T) {
	e := &L3TrajectoryEvaluator{mode: "exact"}
	res, _ := e.Evaluate(nil, &EvalCase{})
	if res.Passed {
		t.Fatal("expected fail for nil trajectory")
	}
}

// ─── IncidentToEval ───────────────────────────────────────────────────────────

func TestIncidentToEval_Convert_NotFound(t *testing.T) {
	i2e := &IncidentToEval{
		incidentStore: &IncidentStore{incidents: []*Incident{}},
		evalStore:     &EvalStore{},
	}
	_, err := i2e.Convert("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing incident")
	}
	var perr *perrors.Error
	if !isPerror(err, &perr) || perr.Code != perrors.CodeNotFound {
		t.Errorf("expected CodeNotFound, got: %v", err)
	}
}

func TestIncidentToEval_Convert_Success(t *testing.T) {
	inc := &Incident{
		ID:               "INC-001",
		Title:            "Search tool returned empty",
		TriggerInput:     "find me files",
		FailedToolCall:   "search",
		SeverityLevel:    1,
		ExpertAnnotation: "should call list_files first",
		Status:           "unresolved",
	}
	i2e := &IncidentToEval{
		incidentStore: &IncidentStore{incidents: []*Incident{inc}},
		evalStore:     &EvalStore{},
	}
	ec, err := i2e.Convert("INC-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ec.ID != "incident-INC-001" {
		t.Errorf("wrong ID: %s", ec.ID)
	}
	if ec.Source != "incident" {
		t.Errorf("wrong source: %s", ec.Source)
	}
	if inc.Status != "converted" {
		t.Errorf("incident status not updated")
	}
	if len(ec.Evaluators) < 2 {
		t.Errorf("expected at least 2 evaluators, got %d", len(ec.Evaluators))
	}
	if ec.CreatedAt == 0 {
		t.Error("CreatedAt should be set")
	}
}

func TestIncidentToEval_NilStore(t *testing.T) {
	i2e := &IncidentToEval{incidentStore: nil, evalStore: &EvalStore{}}
	_, err := i2e.Convert("x")
	if err == nil {
		t.Fatal("expected error for nil incidentStore")
	}
}

// isPerror 辅助函数：判断 error 是否为 *perrors.Error 并赋值。
func isPerror(err error, out **perrors.Error) bool {
	if err == nil {
		return false
	}
	var pe *perrors.Error
	if e, ok := err.(*perrors.Error); ok {
		pe = e
	}
	if pe == nil {
		return false
	}
	*out = pe
	return true
}
