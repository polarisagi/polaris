package codeact

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// TestBuildExecutableScript_DisabledByDefault 验证未显式开启 StatefulSession 时
// 脚本原样返回，不注入任何样板代码——保证未选用此特性的既有调用方行为完全不变。
func TestBuildExecutableScript_DisabledByDefault(t *testing.T) {
	ca := &CodeAct{stateDir: t.TempDir()}
	req := protocol.CodeActRequest{Language: "python", Code: "print(1)", SessionID: "s1"}

	got, err := ca.buildExecutableScript(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != req.Code {
		t.Errorf("expected code unchanged when StatefulSession=false, got %q", got)
	}
}

// TestBuildExecutableScript_NoStateDir 验证 stateDir 未配置时静默降级（不报错）。
func TestBuildExecutableScript_NoStateDir(t *testing.T) {
	ca := &CodeAct{}
	req := protocol.CodeActRequest{Language: "python", Code: "print(1)", SessionID: "s1", StatefulSession: true}

	got, err := ca.buildExecutableScript(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != req.Code {
		t.Errorf("expected code unchanged when stateDir empty, got %q", got)
	}
}

// TestBuildExecutableScript_Python 验证 Python 脚本被正确包裹了状态加载/保存样板，
// 且用户原始代码完整保留在其中。
func TestBuildExecutableScript_Python(t *testing.T) {
	ca := &CodeAct{stateDir: t.TempDir()}
	req := protocol.CodeActRequest{
		Language: "python", Code: "x = 1\nprint(x)",
		SessionID: "session-abc", StatefulSession: true,
	}

	got, err := ca.buildExecutableScript(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "x = 1\nprint(x)") {
		t.Errorf("expected original code preserved verbatim, got:\n%s", got)
	}
	if !strings.Contains(got, "__ca_pickle") || !strings.Contains(got, "__ca_save_state__") {
		t.Errorf("expected pickle-based state harness injected, got:\n%s", got)
	}

	stateDir := filepath.Join(ca.stateDir, codeactStateSubdir)
	if info, statErr := os.Stat(stateDir); statErr != nil || !info.IsDir() {
		t.Errorf("expected state dir %q to be created, stat err: %v", stateDir, statErr)
	}
}

// TestBuildExecutableScript_Bash 验证 Bash 脚本被正确包裹了 source/dump 样板。
func TestBuildExecutableScript_Bash(t *testing.T) {
	ca := &CodeAct{stateDir: t.TempDir()}
	req := protocol.CodeActRequest{
		Language: "bash", Code: "echo hello",
		SessionID: "session-xyz", StatefulSession: true,
	}

	got, err := ca.buildExecutableScript(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "echo hello") {
		t.Errorf("expected original code preserved verbatim, got:\n%s", got)
	}
	if !strings.Contains(got, "source") || !strings.Contains(got, "declare -p") {
		t.Errorf("expected source/declare -p harness injected, got:\n%s", got)
	}
}

// TestSessionStateFile_PathTraversalRejected 验证恶意 SessionID（路径穿越）被净化，
// 不会导致状态文件写到 stateDir 之外。
func TestSessionStateFile_PathTraversalRejected(t *testing.T) {
	ca := &CodeAct{stateDir: t.TempDir()}

	path, err := ca.sessionStateFile("python", "../../etc/passwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedDir := filepath.Join(ca.stateDir, codeactStateSubdir)
	if !strings.HasPrefix(path, expectedDir+string(filepath.Separator)) {
		t.Errorf("expected sanitized path to stay within %q, got %q", expectedDir, path)
	}
	if strings.Contains(path, "..") {
		t.Errorf("expected no path traversal remnants in %q", path)
	}
}

// TestSessionStateFile_EmptyAfterSanitize 验证净化后为空/根路径的 SessionID 被拒绝。
func TestSessionStateFile_EmptyAfterSanitize(t *testing.T) {
	ca := &CodeAct{stateDir: t.TempDir()}
	if _, err := ca.sessionStateFile("python", "/"); err == nil {
		t.Errorf("expected error for session_id sanitizing to root path")
	}
}

// TestStatefulSession_RoundTrip_Python 端到端验证：第一次执行写变量并保存状态，
// 第二次执行（不重新赋值）应能从快照文件恢复该变量——不依赖真实沙箱执行，直接
// 用系统 python3 解释器运行 buildExecutableScript 产出的脚本文本，验证快照文件
// 本身的加载/保存逻辑正确（沙箱执行细节由 Execute() 的既有测试覆盖，不重复）。
func TestStatefulSession_RoundTrip_Python(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available in test environment")
	}
	if err := exec.Command("python3", "-c", "import encodings").Run(); err != nil {
		t.Skip("python3 available but broken (e.g. missing encodings module) in test environment")
	}

	ca := &CodeAct{stateDir: t.TempDir()}
	sessionID := "roundtrip-session"

	step1 := protocol.CodeActRequest{
		Language: "python", Code: "counter = 41", SessionID: sessionID, StatefulSession: true,
	}
	script1, err := ca.buildExecutableScript(step1)
	if err != nil {
		t.Fatalf("build script 1 failed: %v", err)
	}
	if err := runPython(t, script1); err != nil {
		t.Fatalf("run script 1 failed: %v", err)
	}

	step2 := protocol.CodeActRequest{
		Language: "python", Code: "counter += 1\nassert counter == 42, counter",
		SessionID: sessionID, StatefulSession: true,
	}
	script2, err := ca.buildExecutableScript(step2)
	if err != nil {
		t.Fatalf("build script 2 failed: %v", err)
	}
	if err := runPython(t, script2); err != nil {
		t.Fatalf("run script 2 failed (state not carried over): %v", err)
	}
}

// runPython 用系统 python3 解释器直接执行脚本文本（不经过沙箱），仅用于验证
// buildExecutableScript 注入的状态快照样板本身的加载/保存逻辑是否正确。
func runPython(t *testing.T, script string) error {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "codeact_stateful_test_*.py")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	out, err := exec.Command("python3", f.Name()).CombinedOutput()
	if err != nil {
		return errWithOutput(err, out)
	}
	return nil
}

func errWithOutput(err error, out []byte) error {
	return &pythonRunError{err: err, output: string(out)}
}

type pythonRunError struct {
	err    error
	output string
}

func (e *pythonRunError) Error() string {
	return e.err.Error() + ": " + e.output
}

func (e *pythonRunError) Unwrap() error {
	return e.err
}
