package project_preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/job_kill"
	"github.com/polarisagi/polaris/internal/tool/builtin/job_status"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestDetectLauncher_UsesLoopbackAndStrictPort(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher, ok := detectLauncher(root)
	if !ok {
		t.Fatal("static Python launcher not detected")
	}
	command := launcher(45678)
	if !strings.Contains(command, "127.0.0.1") || !strings.Contains(command, "45678") {
		t.Fatalf("launcher is not loopback-bound: %q", command)
	}
}

func TestDetectLauncher_RejectsGenericDevScript(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules", ".bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"scripts":{"dev":"vite"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := detectLauncher(root); ok {
		t.Fatal("must not assume package dev script accepts host and port flags")
	}
}

func TestDetectLauncher_PrefersViteWhenStaticIndexAlsoExists(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("static"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	vite := filepath.Join(bin, "vite")
	if err := os.WriteFile(vite, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	launcher, ok := detectLauncher(root)
	if !ok {
		t.Fatal("Vite launcher not detected")
	}
	command := launcher(40001)
	if !strings.Contains(command, "vite") || !strings.Contains(command, "--strictPort") || strings.Contains(command, "python3") {
		t.Fatalf("Vite must take precedence over static serving: %q", command)
	}
}

func TestPortAvailable_RejectsExistingListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if portAvailable(port) {
		t.Fatalf("occupied port %d reported available", port)
	}
}

func TestProjectPreview_RejectsOccupiedSelectedPortBeforeSpawn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	manager := bash.NewJobManager(t.TempDir())
	_, deps := makePreviewRegistry(t, root, &memoryPreviewStore{values: make(map[string][]byte)}, manager)
	p := &previewTool{deps: deps, active: make(map[string]record), pending: make(map[string]int), portAllocator: func() (int, error) { return port, nil }}
	_, err = p.start(previewContext("preview-port-conflict", true), "preview-port-conflict", root, 1, types.TaintLow)
	if err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("occupied port was not rejected: %v", err)
	}
	jobs, err := manager.ListJobs("preview-port-conflict")
	if err != nil || len(jobs) != 0 {
		t.Fatalf("occupied port check spawned a job: jobs=%+v err=%v", jobs, err)
	}
}

func TestStorageKey_BindsSessionRootAndJob(t *testing.T) {
	key := storageKey("s1", "/workspace/a", "j1")
	if key == storageKey("s2", "/workspace/a", "j1") || key == storageKey("s1", "/workspace/b", "j1") || key == storageKey("s1", "/workspace/a", "j2") {
		t.Fatal("storage key does not bind the complete preview identity")
	}
}

func TestStaticPreview_DeniesSymlinkEscape(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "outside-secret-content"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", staticPythonServer, fmt.Sprint(port), root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	client := &http.Client{Timeout: 300 * time.Millisecond}
	var resp *http.Response
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		resp, err = client.Get(fmt.Sprintf("http://127.0.0.1:%d/%%6cink/secret.txt", port))
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("local preview did not respond: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), secret) {
		t.Fatalf("symlink escape response status=%d body=%q", resp.StatusCode, body)
	}
}

func TestReserve_EnforcesOnePreviewPerSession(t *testing.T) {
	p := &previewTool{active: map[string]record{"job1": {SessionID: "session-a"}}, pending: make(map[string]int)}
	if p.reserve("session-a") {
		t.Fatal("same session must not start a second preview")
	}
	if !p.reserve("session-b") {
		t.Fatal("another session should remain eligible")
	}
	p.releaseReservation("session-b")
}

func TestPruneFinished_ReleasesSlotAndKeepsTerminalState(t *testing.T) {
	jobs := bash.NewJobManager(t.TempDir())
	meta, err := jobs.StartJob(context.Background(), "exit 0", t.TempDir(), "session-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		meta, err = jobs.GetJob(meta.JobID)
		if err == nil && meta.Status != bash.JobStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || meta.Status == bash.JobStatusRunning {
		t.Fatalf("test job did not finish: meta=%+v err=%v", meta, err)
	}
	entry := record{SessionID: "session-a", Root: "/workspace", JobID: meta.JobID, Port: 35001, StartedAt: time.Now().UTC()}
	store := &memoryPreviewStore{values: map[string][]byte{}}
	value, _ := json.Marshal(entry)
	key := storageKey(entry.SessionID, entry.Root, entry.JobID)
	if err := store.Put(context.Background(), []byte(key), value); err != nil {
		t.Fatal(err)
	}
	p := &previewTool{deps: Deps{Store: store, JobManager: jobs}, active: map[string]record{meta.JobID: entry}, pending: map[string]int{}}
	p.pruneFinished(context.Background())
	if p.isActive(meta.JobID) || !p.reserve("session-a") {
		t.Fatal("terminal job must release the session slot")
	}
	stored, err := store.Get(context.Background(), []byte(key))
	if err != nil {
		t.Fatal("terminal binding should remain queryable", err)
	}
	var got record
	if err := json.Unmarshal(stored, &got); err != nil || got.State != string(meta.Status) {
		t.Fatalf("terminal status was not preserved: %+v err=%v", got, err)
	}
}

func TestProjectPreview_RealRegistrySessionBoundStartStatusStop(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>local</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &memoryPreviewStore{values: make(map[string][]byte)}
	manager := bash.GetDefaultJobManager()
	registry, _ := makePreviewRegistry(t, root, store, manager)
	manager.CleanupSessionJobs("preview-test-session")
	start := callPreview(t, registry, previewContext("preview-test-session", true), map[string]any{"action": "start", "path": root, "timeout": 10})
	if start.Status != "ready" || start.JobID == "" || !strings.HasPrefix(start.URL, "http://127.0.0.1:") {
		t.Fatalf("preview did not become ready on loopback: %+v", start)
	}
	t.Cleanup(func() { manager.CleanupSessionJobs("preview-test-session") })

	stranger, err := registry.ExecuteTool(previewContext("preview-stranger", true), "project_preview", mustJSON(t, map[string]any{"action": "status", "path": root, "job_id": start.JobID}), types.TaintLow)
	if err == nil && stranger != nil && stranger.Success {
		t.Fatalf("another session read a preview: %+v", stranger)
	}
	status := callPreview(t, registry, previewContext("preview-test-session", true), map[string]any{"action": "status", "path": root, "job_id": start.JobID})
	if status.Status != string(bash.JobStatusRunning) {
		t.Fatalf("status=%+v", status)
	}
	stopped := callPreview(t, registry, previewContext("preview-test-session", true), map[string]any{"action": "stop", "path": root, "job_id": start.JobID})
	if stopped.Status != "stopped" {
		t.Fatalf("stop=%+v", stopped)
	}
	meta, err := manager.GetJob(start.JobID)
	if err != nil || meta.Status != bash.JobStatusKilled {
		t.Fatalf("job was not stopped through the registry: meta=%+v err=%v", meta, err)
	}
}

func TestProjectPreview_RefusesMissingNestedAuthorizer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := bash.GetDefaultJobManager()
	session := "preview-no-authorizer"
	manager.CleanupSessionJobs(session)
	registry, _ := makePreviewRegistry(t, root, &memoryPreviewStore{values: make(map[string][]byte)}, manager)
	ctx := previewContext(session, false)
	result, err := registry.ExecuteTool(ctx, "project_preview", mustJSON(t, map[string]any{"action": "start", "path": root}), types.TaintLow)
	if err == nil && result != nil && result.Success {
		t.Fatalf("start without fresh nested authorization succeeded: %+v", result)
	}
	jobs, err := manager.ListJobs(session)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("unauthorized preview spawned a job: jobs=%+v err=%v", jobs, err)
	}
}

func TestProjectPreview_CancellationKillsOnlyBoundJob(t *testing.T) {
	root := t.TempDir()
	store := &memoryPreviewStore{values: make(map[string][]byte)}
	manager := bash.NewJobManager(t.TempDir())
	_, deps := makePreviewRegistry(t, root, store, manager)
	meta, err := manager.StartJob(context.Background(), "sleep 15", root, "preview-cancel-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	entry := record{SessionID: "preview-cancel-session", Root: root, JobID: meta.JobID, Port: port, StartedAt: time.Now().UTC()}
	preview := &previewTool{deps: deps, active: map[string]record{meta.JobID: entry}, pending: make(map[string]int)}
	ctx, cancel := context.WithCancel(previewContext(entry.SessionID, true))
	timer := time.AfterFunc(150*time.Millisecond, cancel)
	defer timer.Stop()
	_, err = preview.persistAndAwait(ctx, entry, 10, types.TaintMedium)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("readiness cancellation was not reported: %v", err)
	}
	finished, err := manager.GetJob(meta.JobID)
	if err != nil || finished.Status != bash.JobStatusKilled || preview.isActive(meta.JobID) {
		t.Fatalf("cancel did not stop bound job: meta=%+v active=%v err=%v", finished, preview.isActive(meta.JobID), err)
	}
}

func makePreviewRegistry(t *testing.T, root string, store protocol.Store, manager *bash.JobManager) (*tool.InMemoryToolRegistry, Deps) {
	t.Helper()
	cfg := config.DefaultThresholds()
	sbx := sandbox.NewInProcessSandbox(cfg.M7Tool)
	profile := policy.ProfilePolicy(policy.ProfileFull)
	sbx.Register("bash", bash.MakeBashFn([]string{root}, false, protocol.NetPolicyDeny, "", profile))
	sbx.Register("job_status", job_status.MakeJobStatusFn(manager))
	sbx.Register("job_kill", job_kill.MakeJobKillFn(manager))
	client := network.NewLoopbackSafeHTTPClient(cfg.M11Policy)
	deps := Deps{AllowedPaths: []string{root}, Registry: nil, Store: store, SafeHTTPClient: client, JobManager: manager}
	env := sandbox.NewExecEnvelope(previewTestPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "linux", previewTestTokenVerifier{}).WithExecutionAuditor(previewTestAuditor{})
	registry := tool.NewInMemoryToolRegistry(env, cfg.M7Tool)
	for _, entry := range []types.Tool{
		{Name: "bash", Capability: types.CapWriteLocal, RiskLevel: types.RiskHigh, SandboxTier: types.SandboxInProcess, Source: types.ToolBuiltin, TrustTier: types.TrustSystem},
		{Name: "job_status", Capability: types.CapReadOnly, RiskLevel: types.RiskLow, SandboxTier: types.SandboxInProcess, Source: types.ToolBuiltin, TrustTier: types.TrustSystem},
		{Name: "job_kill", Capability: types.CapWriteLocal, RiskLevel: types.RiskMedium, SandboxTier: types.SandboxInProcess, Source: types.ToolBuiltin, TrustTier: types.TrustSystem},
		{Name: "project_preview", Capability: types.CapWriteLocal, RiskLevel: types.RiskHigh, SandboxTier: types.SandboxInProcess, Source: types.ToolBuiltin, TrustTier: types.TrustSystem},
	} {
		if err := registry.Register(entry); err != nil {
			t.Fatal(err)
		}
	}
	deps.Registry = registry
	sbx.RegisterRich("project_preview", MakeProjectPreviewFn(deps), types.TaintHigh)
	return registry, deps
}

func callPreview(t *testing.T, registry *tool.InMemoryToolRegistry, ctx context.Context, args map[string]any) response {
	t.Helper()
	result, err := registry.ExecuteTool(ctx, "project_preview", mustJSON(t, args), types.TaintLow)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.Success || result.TaintLevel != types.TaintHigh {
		t.Fatalf("preview tool failed or lowered taint: %+v", result)
	}
	var got response
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func previewContext(session string, withAuthorizer bool) context.Context {
	ctx := protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit)
	ctx = context.WithValue(ctx, protocol.CtxSessionIDKey{}, session)
	ctx = context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, &token.Token{})
	if withAuthorizer {
		ctx = protocol.WithToolCallAuthorizer(ctx, previewApprovalAuthorizer{})
	}
	return ctx
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type previewApprovalAuthorizer struct{}

func (previewApprovalAuthorizer) AuthorizeCall(context.Context, types.Tool, []byte, types.TaintLevel) (protocol.PermissionGrant, string, bool) {
	return protocol.GrantExplicit, "approved by test", true
}

type previewTestPolicyGate struct{}

func (previewTestPolicyGate) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}

func (previewTestPolicyGate) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

type previewTestTokenVerifier struct{}

func (previewTestTokenVerifier) Verify(*token.Token) error { return nil }

type previewTestAuditor struct{}

func (previewTestAuditor) BeginExecution(context.Context, protocol.ExecutionAuditIntent) (string, error) {
	return "test", nil
}

func (previewTestAuditor) CompleteExecution(context.Context, string, bool, string) error { return nil }

type memoryPreviewStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (s *memoryPreviewStore) Get(_ context.Context, key []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[string(key)]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), value...), nil
}

func (s *memoryPreviewStore) Put(_ context.Context, key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[string(key)] = append([]byte(nil), value...)
	return nil
}

func (s *memoryPreviewStore) Delete(_ context.Context, key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, string(key))
	return nil
}

func (s *memoryPreviewStore) Scan(_ context.Context, prefix []byte) (protocol.Iterator, error) {
	return &memoryPreviewIterator{}, nil
}

func (s *memoryPreviewStore) BatchWrite(_ context.Context, _ []types.Op) error { return nil }
func (s *memoryPreviewStore) Txn(_ context.Context, fn func(protocol.Transaction) error) error {
	return fn(memoryPreviewTx{s: s})
}
func (s *memoryPreviewStore) Capabilities() types.StoreCapabilities { return types.StoreCapabilities{} }
func (s *memoryPreviewStore) Close() error                          { return nil }

type memoryPreviewIterator struct{}

func (memoryPreviewIterator) Next() bool    { return false }
func (memoryPreviewIterator) Key() []byte   { return nil }
func (memoryPreviewIterator) Value() []byte { return nil }
func (memoryPreviewIterator) Err() error    { return nil }
func (memoryPreviewIterator) Close() error  { return nil }

type memoryPreviewTx struct{ s *memoryPreviewStore }

func (tx memoryPreviewTx) Get(key []byte) ([]byte, error) { return tx.s.Get(context.Background(), key) }
func (tx memoryPreviewTx) Put(key, value []byte) error {
	return tx.s.Put(context.Background(), key, value)
}
func (tx memoryPreviewTx) Delete(key []byte) error { return tx.s.Delete(context.Background(), key) }
func (tx memoryPreviewTx) Scan(prefix []byte) (protocol.Iterator, error) {
	return tx.s.Scan(context.Background(), prefix)
}
