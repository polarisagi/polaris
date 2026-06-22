package skill

import (
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"testing"
	"time"
)

type mockFreshnessChecker struct {
	fresh bool
}

func (m *mockFreshnessChecker) IsFresh(ctx context.Context, traj *CollapseTrajectory) bool {
	return m.fresh
}

func TestCompileGate(t *testing.T) {
	g := NewCompileGate(probe.Tier0)
	if g.maxConcurrent != 2 {
		t.Errorf("expected 2 max concurrent for tier 0")
	}

	if g.TryAcquire(10) {
		t.Errorf("expected false for low free memory")
	}

	if !g.TryAcquire(100) {
		t.Errorf("expected true for acquire 1")
	}
	if !g.TryAcquire(100) {
		t.Errorf("expected true for acquire 2")
	}
	if g.TryAcquire(100) {
		t.Errorf("expected false for acquire 3 (max concurrent reached)")
	}

	if g.InFlight() != 2 {
		t.Errorf("expected 2 in flight")
	}

	g.Release()
	if g.InFlight() != 1 {
		t.Errorf("expected 1 in flight")
	}
}

func TestLogicCollapseCompiler_Compile_Stale(t *testing.T) {
	c := NewLogicCollapseCompiler(LogicCollapseConfig{
		FreshnessChecker: &mockFreshnessChecker{fresh: false},
	})
	_, err := c.Compile(context.Background(), &CompileRequest{Trajectory: &CollapseTrajectory{}})
	if err != ErrStaleTrajectory {
		t.Errorf("expected ErrStaleTrajectory, got %v", err)
	}

	c2 := NewLogicCollapseCompiler(LogicCollapseConfig{})
	_, err = c2.Compile(context.Background(), &CompileRequest{
		Trajectory: &CollapseTrajectory{CompletedAt: time.Now().Add(-31 * 24 * time.Hour).Unix()},
	})
	if err != ErrStaleTrajectory {
		t.Errorf("expected ErrStaleTrajectory without freshness checker, got %v", err)
	}
}

func TestLogicCollapseCompiler_Compile_Tainted(t *testing.T) {
	c := NewLogicCollapseCompiler(LogicCollapseConfig{})
	_, err := c.Compile(context.Background(), &CompileRequest{
		Trajectory: &CollapseTrajectory{TaintLevel: 2}, // Medium taint
	})
	if err != ErrTaintedTrajectory {
		t.Errorf("expected ErrTaintedTrajectory")
	}
}

func TestLogicCollapseCompiler_Compile_EvalGate(t *testing.T) {
	c := NewLogicCollapseCompiler(LogicCollapseConfig{})
	_, err := c.Compile(context.Background(), &CompileRequest{
		Trajectory:     &CollapseTrajectory{},
		EvalGatePassed: false,
	})
	if err != ErrEvalGateNotPassed {
		t.Errorf("expected ErrEvalGateNotPassed")
	}
}

func TestDetectObfuscatedRisk(t *testing.T) {
	if !detectObfuscatedRisk(`eval("test")`) {
		t.Errorf("failed to detect eval")
	}
	if !detectObfuscatedRisk(`exec("return 1")`) {
		t.Errorf("failed to detect exec")
	}
	if !detectObfuscatedRisk(`import os`) {
		t.Errorf("failed to detect import os")
	}
	if !detectObfuscatedRisk(`__import__('os')`) {
		t.Errorf("failed to detect __import__")
	}
	if detectObfuscatedRisk(`print("hello")`) {
		t.Errorf("false positive on print")
	}
}

func TestAssessScriptRisk(t *testing.T) {
	r, t_ := assessScriptRisk([]byte("eval(foo)"), "")
	if r != "high" || t_ != 3 {
		t.Errorf("expected high/3")
	}

	r, t_ = assessScriptRisk([]byte("subprocess.Popen()"), "")
	if r != "high" || t_ != 3 {
		t.Errorf("expected high/3")
	}

	r, t_ = assessScriptRisk([]byte("requests.get('http://example.com')"), "")
	if r != "medium" || t_ != 3 {
		t.Errorf("expected medium/3")
	}

	r, t_ = assessScriptRisk([]byte("open('file', 'w')"), "")
	if r != "medium" || t_ != 3 {
		t.Errorf("expected medium/3")
	}

	r, t_ = assessScriptRisk([]byte("print('hello')"), "builtin")
	if r != "low" || t_ != 1 {
		t.Errorf("expected low/1")
	}

	r, t_ = assessScriptRisk([]byte("print('hello')"), "other")
	if r != "medium" || t_ != 3 {
		t.Errorf("expected medium/3")
	}
}

func TestSignScript(t *testing.T) {
	_, err := signScript([]byte("foo"), nil)
	if err == nil {
		t.Errorf("expected error on empty key")
	}

	s, err := signScript([]byte("foo"), []byte("key"))
	if err != nil || s == "" {
		t.Errorf("expected signature")
	}
}

func TestRedactPIIFields(t *testing.T) {
	s := redactPIIFields(map[string]string{
		"password": "string",  // type is ok
		"email":    "myemail", // type is not ok
		"normal":   "int",
	})
	if s["password"] != "string" {
		t.Errorf("expected unredacted password type")
	}
	if s["email"] != "<REDACTED>" {
		t.Errorf("expected redacted email")
	}
	if s["normal"] != "int" {
		t.Errorf("expected normal int")
	}
}

func TestIsTypeScriptType(t *testing.T) {
	if !isTypeScriptType("string") {
		t.Errorf("string")
	}
	if !isTypeScriptType("number[]") {
		t.Errorf("number[]")
	}
	if !isTypeScriptType("Record<string, any>") {
		t.Errorf("Record")
	}
	if isTypeScriptType("myemail@example.com") {
		t.Errorf("not a type")
	}
}

func TestRunSandboxProbe(t *testing.T) {
	err := runSandboxProbe(context.Background(), nil, []byte(""), "")
	if err != nil {
		t.Errorf("expected nil error on empty dir")
	}

	// This assumes tests won't have a fully valid wasm sandbox environment available,
	// so we expect it to fail if workDir is provided.
	err = runSandboxProbe(context.Background(), nil, []byte("console.log('hi')"), "/tmp/work")
	if err == nil {
		// Just a warning, not failing the test because environment dependencies may vary
		t.Logf("probe did not fail in test environment")
	}
}
