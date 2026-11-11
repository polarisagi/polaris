package project_diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin/run_command"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestSelectLanguage_AutoRequiresDeterministicChoice(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lang, ambiguous, err := selectLanguage(context.Background(), root, "auto", []string{root})
	if err != nil || lang != "auto" || !ambiguous {
		t.Fatalf("selection=(%q,%v,%v), want auto/ambiguous", lang, ambiguous, err)
	}
}

func TestPythonFiles_SkipsVirtualEnvironmentsAndBoundsInput(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".venv"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".venv", "ignored.py"), []byte("invalid !!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, truncated, err := pythonFiles(root, context.Background(), []string{root})
	if err != nil || truncated || len(files) != 1 || filepath.Base(files[0]) != "main.py" {
		t.Fatalf("files=%v truncated=%v err=%v", files, truncated, err)
	}
}

func TestParseDiagnostics_DropsPathsOutsideProject(t *testing.T) {
	root := t.TempDir()
	resolvedRoot := mustEvalSymlinks(t, root)
	raw := filepath.Join(resolvedRoot, "a.py") + ":4:2: error: expected expression\n/elsewhere/leak.py:3:1: error: hidden\n"
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.py"), []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.py"), filepath.Join(root, "escape.py")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	raw += filepath.Join(resolvedRoot, "escape.py") + ":1:1: error: external source\n"
	got := parseDiagnostics(resolvedRoot, "python", raw)
	if len(got) != 1 || got[0].Path != filepath.Join(resolvedRoot, "a.py") || got[0].Line != 4 {
		t.Fatalf("diagnostics=%+v", got)
	}
}

func TestParseRustDiagnostics_DropsExternalSpans(t *testing.T) {
	root := t.TempDir()
	resolvedRoot := mustEvalSymlinks(t, root)
	inside := filepath.Join(resolvedRoot, "src", "main.rs")
	raw := `{"reason":"compiler-message","message":{"level":"error","message":"expected ;","spans":[{"file_name":"` + inside + `","line_start":2,"column_start":3,"is_primary":true}]}}` + "\n" +
		`{"reason":"compiler-message","message":{"level":"error","message":"hidden","spans":[{"file_name":"/outside/leak.rs","line_start":2,"column_start":3,"is_primary":true}]}}`
	got := parseRustDiagnostics(resolvedRoot, raw)
	if len(got) != 1 || got[0].Path != filepath.Join(resolvedRoot, "src", "main.rs") || got[0].Line != 2 || got[0].Severity != "error" {
		t.Fatalf("diagnostics=%+v", got)
	}
}

func TestResultFromCommand_RejectsMissingOutputAndNonIntegerExitCode(t *testing.T) {
	for _, raw := range []string{`{"exit_code":0}`, `{"output":"","exit_code":0.5}`} {
		got, err := resultFromCommand(t.TempDir(), "go", false, []byte(raw))
		if err == nil || got != nil {
			t.Fatalf("malformed result became a successful diagnostic: raw=%s result=%+v err=%v", raw, got, err)
		}
	}
}

func TestProjectDiagnostics_UsesRealRegistryAndGoVet(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "go.mod"), "module example.test/diagnostics\n\ngo 1.20\n")
	writeFixture(t, filepath.Join(root, "bad.go"), "package diagnostics\nfunc value() int { return \"wrong\" }\n")
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	registry, sbx := makeRegisteredDiagnostics(t, root)
	ctx := protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit)
	ctx = context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, &token.Token{})
	authorizer := &grantAuditAuthorizer{}
	ctx = protocol.WithToolCallAuthorizer(ctx, authorizer)
	input := []byte(`{"path":"` + root + `","language":"go","timeout":30}`)
	got := runRegisteredDiagnostics(t, registry, ctx, input)
	if got.Status != "diagnostics" || len(got.Diagnostics) == 0 || !hasPathDiagnostic(got.Diagnostics, filepath.Join(realRoot, "bad.go")) {
		t.Fatalf("expected path-scoped Go vet error, got %+v", got)
	}
	writeFixture(t, filepath.Join(root, "bad.go"), "package diagnostics\nfunc value() int { return 1 }\n")
	got = runRegisteredDiagnostics(t, registry, ctx, input)
	if got.Status != "ok" || len(got.Diagnostics) != 0 {
		t.Fatalf("fixed project should be clean, got %+v", got)
	}
	if authorizer.calls != 2 || authorizer.outerGrant != protocol.GrantNone {
		t.Fatalf("nested call authorization calls=%d grant=%v", authorizer.calls, authorizer.outerGrant)
	}
	if sbx == nil {
		t.Fatal("registered process sandbox unavailable")
	}
}

func TestProjectDiagnostics_PythonSyntaxErrorThenClean(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "main.py"), "def broken(:\n    pass\n")
	registry, _ := makeRegisteredDiagnostics(t, root)
	ctx := protocol.WithPermissionGrant(context.Background(), protocol.GrantAuto)
	ctx = context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, &token.Token{})
	ctx = protocol.WithToolCallAuthorizer(ctx, &grantAuditAuthorizer{})
	input := []byte(`{"path":"` + root + `","language":"python","timeout":20}`)
	got := runRegisteredDiagnostics(t, registry, ctx, input)
	if got.Status != "diagnostics" || !hasPathDiagnostic(got.Diagnostics, filepath.Join(mustEvalSymlinks(t, root), "main.py")) {
		t.Fatalf("expected Python syntax diagnostic, got %+v", got)
	}
	writeFixture(t, filepath.Join(root, "main.py"), "def valid():\n    return 1\n")
	got = runRegisteredDiagnostics(t, registry, ctx, input)
	if got.Status != "ok" || len(got.Diagnostics) != 0 {
		t.Fatalf("fixed Python project should be clean, got %+v", got)
	}
}

func TestProjectDiagnostics_RustOfflineErrorThenClean(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo unavailable")
	}
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "Cargo.toml"), "[package]\nname = \"diagnostics_fixture\"\nversion = \"0.1.0\"\nedition = \"2021\"\n")
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "src", "lib.rs"), "pub fn value() -> i32 { \"wrong\" }\n")
	registry, _ := makeRegisteredDiagnostics(t, root)
	ctx := protocol.WithPermissionGrant(context.Background(), protocol.GrantAuto)
	ctx = context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, &token.Token{})
	ctx = protocol.WithToolCallAuthorizer(ctx, &grantAuditAuthorizer{})
	input := []byte(`{"path":"` + root + `","language":"rust","timeout":60}`)
	got := runRegisteredDiagnostics(t, registry, ctx, input)
	if got.Status != "diagnostics" || !hasPathDiagnostic(got.Diagnostics, filepath.Join(mustEvalSymlinks(t, root), "src", "lib.rs")) {
		t.Fatalf("expected offline Rust compiler diagnostic, got %+v", got)
	}
	writeFixture(t, filepath.Join(root, "src", "lib.rs"), "pub fn value() -> i32 { 1 }\n")
	got = runRegisteredDiagnostics(t, registry, ctx, input)
	if got.Status != "ok" || len(got.Diagnostics) != 0 {
		t.Fatalf("fixed Rust project should be clean, got %+v", got)
	}
}

func TestProjectDiagnostics_RefusesWithoutRerunningOrAcceptingMalformedResult(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "go.mod"), "module example.test/diagnostics\n")
	noAuth := &diagnosticFakeRegistry{commandResult: `{"output":"","exit_code":0}`}
	fn := MakeProjectDiagnosticsFn(Deps{AllowedPaths: []string{root}, Registry: noAuth})
	ctx := protocol.WithPermissionGrant(context.Background(), protocol.GrantAuto)
	toolResult, _ := fn(ctx, sandbox.SandboxSpec{Input: []byte(`{"path":"` + root + `","language":"go"}`), TaintLevel: types.TaintLow})
	if toolResult.Success || toolResult.TaintLevel != types.TaintHigh || noAuth.executeCalls != 0 {
		t.Fatalf("missing authorization must fail before execution and keep High taint: result=%+v calls=%d", toolResult, noAuth.executeCalls)
	}
	var denied result
	if err := json.Unmarshal(toolResult.Output, &denied); err != nil || denied.Status != "denied" || denied.Raw == "" {
		t.Fatalf("denial must include structured actionable output: %+v err=%v", denied, err)
	}

	badResult := &diagnosticFakeRegistry{commandResult: `{"output":"no diagnostics"}`}
	ctx = protocol.WithToolCallAuthorizer(protocol.WithPermissionGrant(context.Background(), protocol.GrantAuto), &grantAuditAuthorizer{})
	fn = MakeProjectDiagnosticsFn(Deps{AllowedPaths: []string{root}, Registry: badResult})
	toolResult, err := fn(ctx, sandbox.SandboxSpec{Input: []byte(`{"path":"` + root + `","language":"go"}`), TaintLevel: types.TaintHigh})
	if err == nil || toolResult.Success || badResult.executeCalls != 1 {
		t.Fatalf("missing exit_code must not become a clean result: result=%+v err=%v calls=%d", toolResult, err, badResult.executeCalls)
	}

	writeFixture(t, filepath.Join(root, "Cargo.toml"), "[package]\nname=\"fixture\"\nversion=\"0.1.0\"\n")
	missingCompiler := &diagnosticFakeRegistry{commandResult: `{"output":"cargo: command not found","exit_code":127}`}
	fn = MakeProjectDiagnosticsFn(Deps{AllowedPaths: []string{root}, Registry: missingCompiler})
	toolResult, err = fn(ctx, sandbox.SandboxSpec{Input: []byte(`{"path":"` + root + `","language":"rust"}`), TaintLevel: types.TaintHigh})
	var unavailable result
	if err != nil || toolResult.Success || json.Unmarshal(toolResult.Output, &unavailable) != nil || unavailable.Status != "unavailable" {
		t.Fatalf("missing compiler must be an unsuccessful unavailable result: result=%+v data=%+v err=%v", toolResult, unavailable, err)
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

type diagnosticFakeRegistry struct {
	executeCalls  int
	commandResult string
}

func (r *diagnosticFakeRegistry) Register(types.Tool) error { return nil }
func (r *diagnosticFakeRegistry) Lookup(name string) (types.Tool, error) {
	if name != "run_command" {
		return types.Tool{}, errors.New("tool not found")
	}
	return types.Tool{Name: name, Capability: types.CapWriteLocal, RiskLevel: types.RiskMedium, Source: types.ToolBuiltin}, nil
}
func (r *diagnosticFakeRegistry) List() []types.Tool { return nil }
func (r *diagnosticFakeRegistry) ExecuteTool(context.Context, string, []byte, types.TaintLevel) (*types.ToolResult, error) {
	r.executeCalls++
	return &types.ToolResult{Success: true, Output: []byte(r.commandResult), TaintLevel: types.TaintHigh}, nil
}

func hasPathDiagnostic(items []Diagnostic, path string) bool {
	for _, item := range items {
		if item.Path == path {
			return true
		}
	}
	return false
}

type grantAuditAuthorizer struct {
	calls      int
	outerGrant protocol.PermissionGrant
}

func (a *grantAuditAuthorizer) AuthorizeCall(ctx context.Context, _ types.Tool, _ []byte, taint types.TaintLevel) (protocol.PermissionGrant, string, bool) {
	a.calls++
	a.outerGrant = protocol.PermissionGrantFrom(ctx)
	if taint < types.TaintHigh || a.outerGrant != protocol.GrantNone {
		return protocol.GrantNone, "unexpected nested authorization context", false
	}
	return protocol.GrantAuto, "", true
}

func makeRegisteredDiagnostics(t *testing.T, root string) (*tool.InMemoryToolRegistry, *sandbox.InProcessSandbox) {
	t.Helper()
	cfg := config.DefaultThresholds().M7Tool
	sbx := sandbox.NewInProcessSandbox(cfg)
	sbx.Register("run_command", run_command.MakeRunCommandFn([]string{root}, false, protocol.NetPolicyDeny, "", policy.StandardPolicy()))
	gate := diagnosticsTestPolicyGate{}
	envelope := sandbox.NewExecEnvelope(gate, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "linux", diagnosticsTestTokenVerifier{}).WithExecutionAuditor(diagnosticsTestAuditor{})
	registry := tool.NewInMemoryToolRegistry(envelope, cfg)
	for _, spec := range []struct {
		name       string
		capability types.CapabilityLevel
	}{
		{name: "run_command", capability: types.CapWriteLocal},
		{name: "project_diagnostics", capability: types.CapWriteLocal},
	} {
		if err := registry.Register(types.Tool{Name: spec.name, Capability: spec.capability, RiskLevel: types.RiskMedium, SandboxTier: types.SandboxInProcess, Source: types.ToolBuiltin, TrustTier: types.TrustSystem, Timeout: 120 * time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	sbx.RegisterRich("project_diagnostics", MakeProjectDiagnosticsFn(Deps{AllowedPaths: []string{root}, Registry: registry}), types.TaintHigh)
	return registry, sbx
}

func runRegisteredDiagnostics(t *testing.T, registry *tool.InMemoryToolRegistry, ctx context.Context, input []byte) result {
	t.Helper()
	toolResult, err := registry.ExecuteTool(ctx, "project_diagnostics", input, types.TaintHigh)
	if err != nil {
		t.Fatal(err)
	}
	if toolResult == nil || !toolResult.Success || toolResult.TaintLevel < types.TaintHigh {
		t.Fatalf("diagnostics tool failed or lowered taint: %+v", toolResult)
	}
	var got result
	if err := json.Unmarshal(toolResult.Output, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func writeFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

type diagnosticsTestPolicyGate struct{}

type diagnosticsTestTokenVerifier struct{}

type diagnosticsTestAuditor struct{}

func (diagnosticsTestAuditor) BeginExecution(context.Context, protocol.ExecutionAuditIntent) (string, error) {
	return "test", nil
}

func (diagnosticsTestAuditor) CompleteExecution(context.Context, string, bool, string) error {
	return nil
}

func (diagnosticsTestTokenVerifier) Verify(*token.Token) error { return nil }

func (diagnosticsTestPolicyGate) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}

func (diagnosticsTestPolicyGate) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}
