package harness

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/eval/util"
	"github.com/polarisagi/polaris/internal/protocol"
)

type mockAgent struct {
	output []byte
	tools  []string
	err    error
}

func (m *mockAgent) Run(ctx context.Context, input []byte) ([]byte, []string, error) {
	return m.output, m.tools, m.err
}

type inputRecordingAgent struct {
	input  []byte
	output []byte
}

func (a *inputRecordingAgent) Run(_ context.Context, input []byte) ([]byte, []string, error) {
	a.input = append([]byte(nil), input...)
	return a.output, nil, nil
}

type codePatchEvaluatorFunc func(context.Context, protocol.EvalCase, []byte) (bool, error)

func (f codePatchEvaluatorFunc) EvaluateCodePatch(ctx context.Context, c protocol.EvalCase, patch []byte) (bool, error) {
	return f(ctx, c, patch)
}

type mockProvider struct {
	protocol.Provider
	resp *types.ProviderResponse
	err  error
}

func (m *mockProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return m.resp, m.err
}

// 返回私钥交给 InjectEvalPrivKey 现签：VerifyRequest 用校验瞬间的 Unix 秒重建
// 签名消息，预签名一旦跨秒即失配（-race/-shuffle 下实测偶发）。
func testSignRunner() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	return pub, priv
}

func TestRunner(t *testing.T) {
	c1 := `{"id":"1", "level":1, "expected":{"output":"hello"}}`
	c2 := `{"id":"2", "level":1, "expected":{"tools":["t1"]}}`
	c3 := `{"id":"3", "level":4, "severity":"P0", "falsifiability_score":0.9}`

	ms := &mockSQLiteStore{vals: [][]byte{[]byte(c1), []byte(c2), []byte(c3)}}

	pub, priv := testSignRunner()
	evalStore := NewSQLiteEvalStore(ms, control.NewEngine(map[string]ed25519.PublicKey{control.RoleM9Optimizer: pub}))

	runner := NewRunner(ms, evalStore, config.DefaultThresholds(), config.EvalConfig{})
	runner.InjectEvalPrivKey(priv)

	ch := make(chan types.EvalCompletedPayload, 1)
	runner.SetEvalChannel(ch)

	agent := &mockAgent{output: []byte("hello world"), tools: []string{"t1"}}
	runner.InjectAgent(agent)

	provider := &mockProvider{
		resp: &types.ProviderResponse{Content: `{"relevance":10,"accuracy":10,"safety":10,"completeness":10,"passed":true,"reason":"ok"}`},
	}
	runner.InjectProvider(provider)

	report, err := runner.RunSuite(context.Background(), "training", "cand1")
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalCases != 3 {
		t.Errorf("expected 3 cases, got %d", report.TotalCases)
	}

	select {
	case payload := <-ch:
		if payload.Suite != "training" {
			t.Errorf("expected training suite")
		}
	case <-time.After(time.Second):
		t.Errorf("timeout waiting for payload")
	}

	// Test Cancel
	runner.activeRuns["run_1"] = func() {}
	err = runner.Cancel(context.Background(), "run_1")
	if err != nil {
		t.Fatal(err)
	}
	err = runner.Cancel(context.Background(), "run_2")
	if err == nil {
		t.Errorf("expected error canceling non-existent run")
	}
}

func TestRunBenchmarkDataset_RealCodePatchAndCounters(t *testing.T) {
	caseData := protocol.EvalCase{ID: "local-1", Description: "fix the bug", BehaviorType: protocol.BehaviorCodePatch,
		Input:    map[string]any{"repo_path": "/tmp/local", "base_commit": strings.Repeat("a", 40), "source_files": map[string]any{"main.go": "package main"}, "test_patch": "SECRET TEST"},
		Expected: map[string]any{"criteria": "SECRET EXPECTATION"}, Config: map[string]any{"fail_to_pass_commands": []string{"RUNNER_ONLY_FAIL_TO_PASS"}, "pass_to_pass_commands": []string{"RUNNER_ONLY_PASS_TO_PASS"}}}
	candidate := &inputRecordingAgent{output: []byte("CANDIDATE PATCH")}
	called := false
	runner := NewRunner(nil, nil, config.DefaultThresholds(), config.EvalConfig{})
	runner.InjectCodePatchAgent(candidate)
	runner.InjectCodePatchEvaluator(codePatchEvaluatorFunc(func(_ context.Context, got protocol.EvalCase, patch []byte) (bool, error) {
		called = true
		if got.ID != caseData.ID || string(patch) != "CANDIDATE PATCH" {
			t.Fatalf("unexpected evaluator args: %s %q", got.ID, patch)
		}
		return true, nil
	}))
	report, err := runner.RunBenchmarkDataset(context.Background(), "project", []any{caseData}, "test")
	if err != nil {
		t.Fatal(err)
	}
	var modelInput map[string]any
	if err := json.Unmarshal(candidate.input, &modelInput); err != nil {
		t.Fatal(err)
	}
	if _, ok := modelInput["test_patch"]; ok {
		t.Fatal("trusted test_patch was sent to candidate agent")
	}
	if strings.Contains(string(candidate.input), "SECRET EXPECTATION") {
		t.Fatal("expected data was sent to candidate agent")
	}
	for _, hidden := range []string{"RUNNER_ONLY_FAIL_TO_PASS", "RUNNER_ONLY_PASS_TO_PASS"} {
		if strings.Contains(string(candidate.input), hidden) {
			t.Fatalf("test configuration leaked to candidate agent: %s", hidden)
		}
	}
	if string(candidate.input) == "" || !called {
		t.Fatal("candidate agent or evaluator was not called")
	}
	if report.TotalCases != 1 || report.PassCount != 1 || report.Status != "completed" {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestRunBenchmarkDataset_RejectsEmptyAndUnsupported(t *testing.T) {
	runner := NewRunner(nil, nil, config.DefaultThresholds(), config.EvalConfig{})
	if _, err := runner.RunBenchmarkDataset(context.Background(), "empty", nil, ""); err == nil {
		t.Fatal("empty dataset must fail")
	}
	report, err := runner.RunBenchmarkDataset(context.Background(), "unsupported", []any{protocol.EvalCase{BehaviorType: protocol.BehaviorCodePatch}}, "")
	if err == nil || report == nil || report.Status != "failed" {
		t.Fatalf("unsupported case must fail explicitly: report=%+v err=%v", report, err)
	}
	valid := protocol.EvalCase{ID: "no-executor", Description: "problem", BehaviorType: protocol.BehaviorCodePatch,
		Input:  map[string]any{"repo_path": "/tmp/repo", "base_commit": strings.Repeat("d", 40), "source_files": map[string]any{"a.go": "package a"}},
		Config: map[string]any{"fail_to_pass_commands": []string{"go test"}, "pass_to_pass_commands": []string{"go test"}}}
	report, err = runner.RunBenchmarkDataset(context.Background(), "no-executor", []any{valid}, "")
	if err != nil || report == nil || report.PassCount != 0 || report.FailCount != 1 || report.Status != "failed" {
		t.Fatalf("missing candidate/evaluator must fail closed: report=%+v err=%v", report, err)
	}
}

func TestRunBenchmarkDataset_CancelledAfterLastEvaluatorIsNotCompleted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := NewRunner(nil, nil, config.DefaultThresholds(), config.EvalConfig{})
	runner.InjectCodePatchAgent(&inputRecordingAgent{output: []byte("candidate")})
	runner.InjectCodePatchEvaluator(codePatchEvaluatorFunc(func(context.Context, protocol.EvalCase, []byte) (bool, error) {
		cancel()
		return true, nil
	}))
	caseData := protocol.EvalCase{ID: "cancel-last", Description: "problem", BehaviorType: protocol.BehaviorCodePatch,
		Input:  map[string]any{"repo_path": "/tmp/repo", "base_commit": strings.Repeat("c", 40), "source_files": map[string]any{"a.go": "package a"}},
		Config: map[string]any{"fail_to_pass_commands": []string{"go test"}, "pass_to_pass_commands": []string{"go test"}}}
	report, err := runner.RunBenchmarkDataset(ctx, "project", []any{caseData}, "test")
	if err == nil || report == nil || report.Status != "cancelled" {
		t.Fatalf("cancelled final case was misreported: report=%+v err=%v", report, err)
	}
}

func TestRunBenchmarkDataset_UnjudgedCaseFailsMixedRun(t *testing.T) {
	runner := NewRunner(nil, nil, config.DefaultThresholds(), config.EvalConfig{})
	runner.InjectCodePatchAgent(&inputRecordingAgent{output: []byte("candidate")})
	runner.InjectCodePatchEvaluator(codePatchEvaluatorFunc(func(context.Context, protocol.EvalCase, []byte) (bool, error) {
		return true, nil
	}))
	patchCase := protocol.EvalCase{ID: "known-pass", Description: "fix", BehaviorType: protocol.BehaviorCodePatch,
		Input:  map[string]any{"repo_path": "/tmp/repo", "base_commit": strings.Repeat("b", 40), "source_files": map[string]any{"a.go": "package a"}},
		Config: map[string]any{"fail_to_pass_commands": []string{"go test"}, "pass_to_pass_commands": []string{"go test"}}}
	unjudgedCase := EvalCase{ID: "unjudged", Level: Level4LLMJudge, Severity: SeverityP1,
		Input: map[string]any{"text": "ok"}, Expected: map[string]any{"output": "ok"}}
	report, err := runner.RunBenchmarkDataset(context.Background(), "mixed-unjudged", []any{patchCase, unjudgedCase}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if report.PassCount != 1 || report.FailCount != 1 || report.L4Unjudged != 1 || report.Status != "failed" {
		t.Fatalf("unjudged mixed benchmark was not failed: %+v", report)
	}
}

func TestExtractJSON(t *testing.T) {
	s := util.ExtractJSON("```json\n{\"a\":1}\n```")
	if s != `{"a":1}` {
		t.Errorf("expected json object, got %s", s)
	}
}

type mockSQLiteStore struct {
	protocol.Store
	vals [][]byte
}

func (m *mockSQLiteStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return &mockIterator{values: m.vals}, nil
}
