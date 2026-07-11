package codeact

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── mock 桩 ────────────────────────────────────────────────────────────────

type mockSandbox struct {
	level  int
	result *types.ToolResult
	err    error
}

func (m *mockSandbox) Run(_ context.Context, _ sandbox.SandboxSpec) (*types.ToolResult, error) {
	return m.result, m.err
}

func (m *mockSandbox) Level() int { return m.level }

type mockPolicyGate struct {
	allowed bool
	err     error
}

func (m *mockPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return m.allowed, m.err
}

func (m *mockPolicyGate) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{}, nil
}

type mockGovAgent struct {
	err error
}

func (m *mockGovAgent) ValidateCode(_ string, _ []byte, _ map[string]bool) error {
	return m.err
}

// mockTokenManager satisfies the tokenManager interface used by CodeAct.
type mockTokenManager struct {
	tok *token.Token
	err error
}

func (m *mockTokenManager) Lookup(_ string) (*token.Token, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.tok != nil {
		return m.tok, nil
	}
	return &token.Token{}, nil
}

func (m *mockTokenManager) Verify(_ *token.Token) error { return m.err }

// defaultMockTokenManager returns a token manager that always succeeds.
func defaultMockTokenManager() *mockTokenManager { return &mockTokenManager{} }

type mockToolExecutor struct{}

func (m *mockToolExecutor) Execute(_ context.Context, _ types.ToolCallRequest) (*types.ToolResult, error) {
	return nil, nil
}

func (m *mockToolExecutor) ExecuteDryRun(_ context.Context, _ types.ToolCallRequest) (*types.ToolResult, error) {
	return nil, nil
}

func (m *mockToolExecutor) RecordAudit(_ context.Context, _ string, _ []byte) error { return nil }
func (m *mockToolExecutor) Cancel(_ context.Context, _ string) error                { return nil }

// ── writeTempScript ────────────────────────────────────────────────────────

func TestWriteTempScript_Python(t *testing.T) {
	code := "print('hello')"
	path, err := writeTempScript("python", code)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(path)

	if !strings.HasSuffix(path, ".py") {
		t.Errorf("expected .py suffix, got %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if string(data) != code {
		t.Errorf("file content mismatch: got %q", string(data))
	}
}

func TestWriteTempScript_Bash(t *testing.T) {
	code := "echo hello"
	path, err := writeTempScript("bash", code)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(path)

	if !strings.HasSuffix(path, ".sh") {
		t.Errorf("expected .sh suffix, got %q", path)
	}
	// bash 脚本必须有执行权限（0700）
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat temp file: %v", err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Errorf("bash script not executable: mode=%o", fi.Mode())
	}
}

// ── DefaultASTChecker.CheckPython ──────────────────────────────────────────

func TestASTChecker_CheckPython_Safe(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`
x = 1 + 2
print(x)
`)
	if err := checker.CheckPython(code); err != nil {
		t.Errorf("expected no error for safe code, got: %v", err)
	}
}

func TestASTChecker_CheckPython_EvalBlocked(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`eval("1+1")`)
	if err := checker.CheckPython(code); err == nil {
		t.Error("expected error for eval(), got nil")
	}
}

func TestASTChecker_CheckPython_ExecBlocked(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`exec("import os")`)
	if err := checker.CheckPython(code); err == nil {
		t.Error("expected error for exec(), got nil")
	}
}

func TestASTChecker_CheckPython_OsSystemBlocked(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`import os; os.system("ls")`)
	if err := checker.CheckPython(code); err == nil {
		t.Error("expected error for os.system(), got nil")
	}
}

func TestASTChecker_CheckPython_SubprocessPopenBlocked(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`import subprocess; subprocess.Popen(["ls"])`)
	if err := checker.CheckPython(code); err == nil {
		t.Error("expected error for subprocess.Popen(), got nil")
	}
}

func TestASTChecker_CheckPython_InvalidSyntax(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`def broken(`)
	if err := checker.CheckPython(code); err == nil {
		t.Error("expected parse error for invalid python, got nil")
	}
}

// ── DefaultASTChecker.CheckBash ────────────────────────────────────────────

func TestASTChecker_CheckBash_Safe(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`echo "hello world"`)
	if err := checker.CheckBash(code); err != nil {
		t.Errorf("expected no error for safe bash, got: %v", err)
	}
}

func TestASTChecker_CheckBash_EvalBlocked(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`eval "rm -rf /"`)
	if err := checker.CheckBash(code); err == nil {
		t.Error("expected error for eval, got nil")
	}
}

func TestASTChecker_CheckBash_ExecBlocked(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`exec bash`)
	if err := checker.CheckBash(code); err == nil {
		t.Error("expected error for exec, got nil")
	}
}

func TestASTChecker_CheckBash_RmRfBlocked(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte(`rm -rf /tmp/test`)
	if err := checker.CheckBash(code); err == nil {
		t.Error("expected error for rm -rf, got nil")
	}
}

func TestASTChecker_CheckBash_InvalidSyntax(t *testing.T) {
	checker := &DefaultASTChecker{}
	code := []byte("if [[") // 不完整的 if 语句
	if err := checker.CheckBash(code); err == nil {
		t.Error("expected parse error for invalid bash, got nil")
	}
}

// ── validateBasic（通过 Execute 覆盖） ─────────────────────────────────────

func TestExecute_EmptyCode(t *testing.T) {
	ca := NewCodeAct(nil, nil)
	_, err := ca.Execute(context.Background(), protocol.CodeActRequest{
		Language:     "python",
		Code:         "",
		CapabilityID: "cap-1",
	})
	if err == nil {
		t.Error("expected error for empty code, got nil")
	}
}

func TestExecute_UnsupportedLanguage(t *testing.T) {
	ca := NewCodeAct(nil, nil)
	_, err := ca.Execute(context.Background(), protocol.CodeActRequest{
		Language:     "ruby",
		Code:         "puts 'hi'",
		CapabilityID: "cap-1",
	})
	if err == nil {
		t.Error("expected error for unsupported language, got nil")
	}
}

func TestExecute_MissingCapabilityID(t *testing.T) {
	ca := NewCodeAct(nil, nil)
	_, err := ca.Execute(context.Background(), protocol.CodeActRequest{
		Language:     "python",
		Code:         "x=1",
		CapabilityID: "",
	})
	if err == nil {
		t.Error("expected error for empty capability_id, got nil")
	}
}

func TestExecute_PolicyDenied(t *testing.T) {
	gate := &mockPolicyGate{allowed: false}
	ca := NewCodeAct(sandbox.NewExecEnvelope(gate, sandbox.NewSandboxRouter(nil, nil, sandbox.NewInProcessSandbox(), "linux", 0), 0, "linux", nil), &mockToolExecutor{},
		WithGovernanceAgent(&mockGovAgent{}),
		WithTokenManager(defaultMockTokenManager()),
	)
	res, err := ca.Execute(context.Background(), protocol.CodeActRequest{
		Language:     "python",
		Code:         "x=1",
		CapabilityID: "cap-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 1 || !strings.Contains(string(res.Output), "policy denied") {
		t.Errorf("expected policy denied in output, got exit=%d output=%q", res.ExitCode, res.Output)
	}
}

func TestExecute_NilPolicyGate_FailClosed(t *testing.T) {
	// policy gate 为 nil → fail-closed (envelope 中的 gate 为 nil)
	// token manager 照常注入（只测 policy gate 缺失路径）
	ca := NewCodeAct(sandbox.NewExecEnvelope(nil, sandbox.NewSandboxRouter(nil, nil, sandbox.NewInProcessSandbox(), "linux", 0), 0, "linux", nil), &mockToolExecutor{},
		WithGovernanceAgent(&mockGovAgent{}),
		WithTokenManager(defaultMockTokenManager()),
	)
	_, err := ca.Execute(context.Background(), protocol.CodeActRequest{
		Language:     "python",
		Code:         "x=1",
		CapabilityID: "cap-1",
	})
	if err == nil || !strings.Contains(err.Error(), "policy gate not initialized") {
		t.Errorf("expected fail-closed error, got %v", err)
	}
}

func TestExecute_SandboxLevelTooLow(t *testing.T) {
	// sandbox level=2 < 需求 L3 → 拒绝
	gate := &mockPolicyGate{allowed: true}
	router := sandbox.NewSandboxRouter(nil, nil, nil, "linux", 0)
	envelope := sandbox.NewExecEnvelope(gate, router, 0, "linux", nil)
	ca := NewCodeAct(envelope, &mockToolExecutor{},
		WithGovernanceAgent(&mockGovAgent{}),
		WithTokenManager(defaultMockTokenManager()),
	)
	_, err := ca.Execute(context.Background(), protocol.CodeActRequest{
		Language:     "python",
		Code:         "x=1",
		CapabilityID: "cap-1",
	})
	if err == nil || !strings.Contains(err.Error(), "isolation unavailable") {
		t.Errorf("expected isolation error, got %v", err)
	}
}

func TestExecute_Success(t *testing.T) {
	gate := &mockPolicyGate{allowed: true}
	sbx := &mockSandbox{
		level:  3,
		result: &types.ToolResult{Output: []byte("hello"), Success: true, LatencyMs: 10},
	}
	router := sandbox.NewSandboxRouter(nil, nil, sbx, "linux", 0)
	envelope := sandbox.NewExecEnvelope(gate, router, 0, "linux", nil)
	exec := &mockToolExecutor{}
	ca := NewCodeAct(envelope, exec,
		WithGovernanceAgent(&mockGovAgent{}),
		WithTokenManager(defaultMockTokenManager()),
	)
	res, err := ca.Execute(context.Background(), protocol.CodeActRequest{
		Language:     "python",
		Code:         "print('hello')",
		CapabilityID: "cap-1",
		SessionID:    "s1",
		AgentID:      "a1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if string(res.Output) != "hello" {
		t.Errorf("output: got %q, want %q", res.Output, "hello")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code: got %d, want 0", res.ExitCode)
	}
}

func TestExecute_MaxCodeSizeLimit(t *testing.T) {
	// 50 bytes limit
	ca := NewCodeAct(nil, nil, WithMaxCodeSize(50))
	req := protocol.CodeActRequest{
		Language:     "python",
		Code:         strings.Repeat("a", 51),
		CapabilityID: "cap-1",
	}

	err := ca.validateBasic(req)
	if err == nil {
		t.Fatal("expected error for exceeding max code size, got nil")
	}
	appErr, ok := err.(*apperr.Error)
	if !ok || appErr.Code != apperr.CodeInvalidInput {
		t.Fatalf("expected CodeInvalidInput error, got %v", err)
	}
	if !strings.Contains(err.Error(), "exceeds maximum limit") {
		t.Fatalf("expected error message to mention size limit, got %v", err)
	}
}
