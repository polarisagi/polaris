package sandbox

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// bareArgvWrapper 是 ArgvWrapper 的测试替身：不经 Rust FFI/bwrap/Seatbelt，
// 直接把 ExecPath/ExecArgs/宿主环境原样透传。真实沙箱封装依赖已编译的 Rust
// dylib，在纯 Go 测试环境里不保证存在；这里的目标是验证 PersistentSandbox
// 自身的会话池/协议逻辑是否正确，不是复测 Rust FFI 桥接（那部分由
// internal/tool/sandbox 的测试和 RustArgvWrapper 的 var _ ArgvWrapper 断言
// 覆盖）。生产环境使用的是 toolsb.NewRustArgvWrapper，见 boot_tools.go。
type bareArgvWrapper struct{}

func (bareArgvWrapper) WrapArgv(_ context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	// 透传宿主关键 env 变量（PATH / PYTHONHOME / CONDA_PREFIX 等），确保
	// Python 解释器能找到标准库（encodings 等）。测试目标是验证会话池协议，
	// 不是测试沙箱封装本身，所以这里不做任何削减。
	keepPrefixes := []string{"PATH=", "PYTHON", "CONDA", "HOME=", "TMPDIR=", "TEMP=", "TMP="}
	var hostEnv []string
	for _, e := range os.Environ() {
		for _, prefix := range keepPrefixes {
			if strings.HasPrefix(e, prefix) {
				hostEnv = append(hostEnv, e)
				break
			}
		}
	}
	return &protocol.WrapArgvResult{
		Executable:    sctx.ExecPath,
		Argv:          sctx.ExecArgs,
		EnvInArgv:     false,
		Env:           append(hostEnv, sctx.EnvExtra...),
		SandboxMethod: "bare_test",
	}, nil
}

// failingArgvWrapper 总是拒绝，用于验证 spawn 失败时 fail-closed（不裸跑）。
type failingArgvWrapper struct{}

func (failingArgvWrapper) WrapArgv(context.Context, protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	return nil, apperr.New(apperr.CodeInternal, "simulated wrap failure")
}

func testConfig() PersistentSandboxConfig {
	return PersistentSandboxConfig{
		IdleTTL:      time.Minute,
		MaxSessions:  4,
		ExecTimeout:  5 * time.Second,
		ReapInterval: time.Minute,
	}
}

func requirePython(t *testing.T) {
	t.Helper()
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		pyPath, err = exec.LookPath("python")
		if err != nil {
			t.Skip("python3/python not found on PATH, skipping")
		}
	}
	// Verify the interpreter actually works in the current environment.
	// On conda-managed systems the LookPath-found python3 may be a conda binary
	// that needs PYTHONHOME set correctly; if it can't import encodings the test
	// would always fail regardless of our code, so skip rather than FAIL.
	out, runErr := exec.Command(pyPath, "-c", "import encodings; print('ok')").CombinedOutput() //nolint:gosec
	if runErr != nil || !strings.Contains(string(out), "ok") {
		t.Skipf("python at %s cannot run basic import (output: %s err: %v); skipping persistent-sandbox tests", pyPath, out, runErr)
	}
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found on PATH, skipping")
	}
}

// ─── Available() ────────────────────────────────────────────────────────────

func TestPersistentSandbox_UnavailableWithoutWrapper(t *testing.T) {
	p := NewPersistentSandbox(nil, testConfig())
	defer p.Shutdown()
	if p.Available() {
		t.Fatal("expected Available()==false when no ArgvWrapper injected (fail-closed)")
	}
}

func TestPersistentSandbox_AvailableWithWrapperAndInterpreter(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if _, err2 := exec.LookPath("bash"); err2 != nil {
			t.Skip("neither python3 nor bash found on PATH, skipping")
		}
	}
	p := NewPersistentSandbox(bareArgvWrapper{}, testConfig())
	defer p.Shutdown()
	if !p.Available() {
		t.Fatal("expected Available()==true when wrapper injected and an interpreter is on PATH")
	}
	if p.Backend() != "live_process_pool" {
		t.Fatalf("unexpected backend label: %q", p.Backend())
	}
}

func TestPersistentSandbox_RunFailClosedWhenUnavailable(t *testing.T) {
	p := NewPersistentSandbox(nil, testConfig())
	defer p.Shutdown()
	_, err := p.Run(context.Background(), SandboxSpec{SessionID: "s1", Language: "python", ScriptBytes: []byte("x=1")})
	if err == nil {
		t.Fatal("expected error when backend unavailable")
	}
	if !apperr.IsCode(err, apperr.CodeUnimplemented) {
		t.Fatalf("expected CodeUnimplemented, got %v", err)
	}
}

func TestPersistentSandbox_RunRequiresSessionID(t *testing.T) {
	requireBash(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, testConfig())
	defer p.Shutdown()
	_, err := p.Run(context.Background(), SandboxSpec{Language: "bash", ScriptBytes: []byte("echo hi")})
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Fatalf("expected CodeInvalidInput for missing SessionID, got %v", err)
	}
}

func TestPersistentSandbox_SpawnFailClosedOnWrapError(t *testing.T) {
	requireBash(t)
	p := NewPersistentSandbox(failingArgvWrapper{}, testConfig())
	defer p.Shutdown()
	_, err := p.Run(context.Background(), SandboxSpec{SessionID: "s1", Language: "bash", ScriptBytes: []byte("echo hi")})
	if err == nil {
		t.Fatal("expected error when ArgvWrapper fails (fail-closed, no bare exec fallback)")
	}
}

// ─── 真实跨调用状态持久化（核心价值验证）────────────────────────────────────

func TestPersistentSandbox_Python_StatePersistsAcrossCalls(t *testing.T) {
	requirePython(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, testConfig())
	defer p.Shutdown()

	ctx := context.Background()
	res1, err := p.Run(ctx, SandboxSpec{
		SessionID: "py-session-1", Language: "python",
		ScriptBytes: []byte("x = 42\nprint('set')"),
		ExtraEnv:    os.Environ(),
	})
	if err != nil {
		t.Fatalf("call1 failed: %v", err)
	}
	if !res1.Success || !strings.Contains(string(res1.Output), "set") {
		t.Fatalf("call1 unexpected result: success=%v output=%q error=%q", res1.Success, res1.Output, res1.Error)
	}

	res2, err := p.Run(ctx, SandboxSpec{
		SessionID: "py-session-1", Language: "python",
		ScriptBytes: []byte("print(x)"),
		ExtraEnv:    os.Environ(),
	})
	if err != nil {
		t.Fatalf("call2 failed: %v", err)
	}
	if !res2.Success {
		t.Fatalf("call2 unexpected failure: output=%q error=%q", res2.Output, res2.Error)
	}
	if !strings.Contains(string(res2.Output), "42") {
		t.Fatalf("expected call2 to see variable x=42 set by call1 (real process persistence), got output=%q", res2.Output)
	}

	// 验证确实是同一个会话（同一进程）在服务两次调用，而不是碰巧两次都新建。
	if len(p.sessions) != 1 {
		t.Fatalf("expected exactly 1 live session, got %d", len(p.sessions))
	}
}

func TestPersistentSandbox_Python_ExceptionCaptured(t *testing.T) {
	requirePython(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, testConfig())
	defer p.Shutdown()

	res, err := p.Run(context.Background(), SandboxSpec{
		SessionID: "py-session-err", Language: "python",
		ScriptBytes: []byte("raise ValueError('something went wrong')"),
		ExtraEnv:    os.Environ(),
	})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if res.Success {
		t.Fatal("expected Success=false for raised exception")
	}
	if !strings.Contains(res.Error, "ValueError") || !strings.Contains(res.Error, "something went wrong") {
		t.Fatalf("expected error text to contain exception details, got %q", res.Error)
	}

	// 会话在异常后应仍然存活（Python 异常不是协议层错误，不应导致 kill）。
	if len(p.sessions) != 1 {
		t.Fatalf("expected session to survive a caught Python exception, got %d live sessions", len(p.sessions))
	}
}

func TestPersistentSandbox_Bash_StatePersistsAcrossCalls(t *testing.T) {
	requireBash(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, testConfig())
	defer p.Shutdown()

	ctx := context.Background()
	res1, err := p.Run(ctx, SandboxSpec{
		SessionID: "bash-session-1", Language: "bash",
		ScriptBytes: []byte("export FOO=bar"),
	})
	if err != nil {
		t.Fatalf("call1 failed: %v", err)
	}
	if !res1.Success {
		t.Fatalf("call1 unexpected failure: output=%q", res1.Output)
	}

	res2, err := p.Run(ctx, SandboxSpec{
		SessionID: "bash-session-1", Language: "bash",
		ScriptBytes: []byte("echo $FOO"),
	})
	if err != nil {
		t.Fatalf("call2 failed: %v", err)
	}
	if !res2.Success {
		t.Fatalf("call2 unexpected failure: output=%q", res2.Output)
	}
	if !strings.Contains(string(res2.Output), "bar") {
		t.Fatalf("expected call2 to see FOO=bar exported by call1, got output=%q", res2.Output)
	}
}

func TestPersistentSandbox_Bash_NonZeroExitReflectsFailure(t *testing.T) {
	requireBash(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, testConfig())
	defer p.Shutdown()

	res, err := p.Run(context.Background(), SandboxSpec{
		SessionID: "bash-session-fail", Language: "bash",
		ScriptBytes: []byte("false"),
	})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if res.Success {
		t.Fatal("expected Success=false for non-zero bash exit code")
	}
}

// ─── 生命周期管理 ────────────────────────────────────────────────────────────

func TestPersistentSandbox_IdleReapKillsSession(t *testing.T) {
	requireBash(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, PersistentSandboxConfig{
		IdleTTL:      50 * time.Millisecond,
		MaxSessions:  4,
		ExecTimeout:  5 * time.Second,
		ReapInterval: 20 * time.Millisecond,
	})
	defer p.Shutdown()

	_, err := p.Run(context.Background(), SandboxSpec{SessionID: "reap-me", Language: "bash", ScriptBytes: []byte("echo hi")})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	p.mu.Lock()
	n := len(p.sessions)
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected 1 live session right after Run, got %d", n)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n = len(p.sessions)
		p.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected idle session to be reaped within 2s, still have %d live sessions", n)
}

func TestPersistentSandbox_MaxSessionsEvictsOldest(t *testing.T) {
	requireBash(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, PersistentSandboxConfig{
		IdleTTL: time.Minute, MaxSessions: 1, ExecTimeout: 5 * time.Second, ReapInterval: time.Minute,
	})
	defer p.Shutdown()

	ctx := context.Background()
	if _, err := p.Run(ctx, SandboxSpec{SessionID: "old", Language: "bash", ScriptBytes: []byte("echo 1")}); err != nil {
		t.Fatalf("run(old) failed: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // 确保 lastUsed 时间戳有序
	if _, err := p.Run(ctx, SandboxSpec{SessionID: "new", Language: "bash", ScriptBytes: []byte("echo 2")}); err != nil {
		t.Fatalf("run(new) failed: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) != 1 {
		t.Fatalf("expected exactly 1 live session after eviction, got %d", len(p.sessions))
	}
	if _, ok := p.sessions["new"]; !ok {
		t.Fatal("expected newest session 'new' to survive eviction")
	}
	if _, ok := p.sessions["old"]; ok {
		t.Fatal("expected oldest session 'old' to be evicted")
	}
}

func TestPersistentSandbox_ShutdownKillsAllSessions(t *testing.T) {
	requireBash(t)
	p := NewPersistentSandbox(bareArgvWrapper{}, testConfig())

	ctx := context.Background()
	if _, err := p.Run(ctx, SandboxSpec{SessionID: "a", Language: "bash", ScriptBytes: []byte("echo 1")}); err != nil {
		t.Fatalf("run(a) failed: %v", err)
	}
	if _, err := p.Run(ctx, SandboxSpec{SessionID: "b", Language: "bash", ScriptBytes: []byte("echo 2")}); err != nil {
		t.Fatalf("run(b) failed: %v", err)
	}

	p.Shutdown()

	p.mu.Lock()
	n := len(p.sessions)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected 0 sessions after Shutdown, got %d", n)
	}
}

func TestPersistentSandbox_ShutdownNilSafe(t *testing.T) {
	var p *PersistentSandbox
	p.Shutdown() // 不应 panic
}

// ─── SandboxRouter 降级链（沿用既有约定，Available()==false 时按 Container 降级）──

func TestSandboxRouter_PersistentTier_FallsBackToContainer(t *testing.T) {
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	container := NewContainerSandbox("bwrap", runtime.GOOS, 2, nil, config.DefaultThresholds().M7Tool)
	router := NewSandboxRouter(inProc, container, nil, runtime.GOOS, 2)
	unavailable := NewPersistentSandbox(nil, testConfig()) // wrapper=nil → Available()==false
	defer unavailable.Shutdown()
	router.WithPersistent(unavailable)

	provider, err := router.RouteByTier(types.SandboxPersistent, types.TrustSystem)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != SandboxProvider(container) {
		t.Fatalf("expected fallback to container provider, got %T", provider)
	}
}

func TestSandboxRouter_PersistentTier_RoutesToPersistentWhenAvailable(t *testing.T) {
	requireBash(t)
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	container := NewContainerSandbox("bwrap", runtime.GOOS, 2, nil, config.DefaultThresholds().M7Tool)
	router := NewSandboxRouter(inProc, container, nil, runtime.GOOS, 2)
	persistent := NewPersistentSandbox(bareArgvWrapper{}, testConfig())
	defer persistent.Shutdown()
	router.WithPersistent(persistent)

	provider, err := router.RouteByTier(types.SandboxPersistent, types.TrustSystem)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != SandboxProvider(persistent) {
		t.Fatalf("expected persistent provider to be selected when available, got %T", provider)
	}
}

func TestSandboxRouter_PersistentTier_NoFallbackFailsClosed(t *testing.T) {
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 2)
	unavailable := NewPersistentSandbox(nil, testConfig())
	defer unavailable.Shutdown()
	router.WithPersistent(unavailable)

	_, err := router.RouteByTier(types.SandboxPersistent, types.TrustSystem)
	if err == nil {
		t.Fatal("expected fail-closed error when no persistent/container/remote backend is available")
	}
	if !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("expected CodeForbidden, got %v", err)
	}
}

func TestSandboxRouter_PersistentTier_WithoutInjection(t *testing.T) {
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	container := NewContainerSandbox("bwrap", runtime.GOOS, 2, nil, config.DefaultThresholds().M7Tool)
	router := NewSandboxRouter(inProc, container, nil, runtime.GOOS, 2)

	provider, err := router.RouteByTier(types.SandboxPersistent, types.TrustSystem)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != SandboxProvider(container) {
		t.Fatalf("expected fallback to container provider, got %T", provider)
	}
	if router.PersistentAvailable() {
		t.Fatal("expected PersistentAvailable()==false when WithPersistent was never called")
	}
}
