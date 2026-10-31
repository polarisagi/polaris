package hook

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type hookAuditRecord struct {
	id                     string
	tool, source, argsHash string
	argsLength             int
	success                bool
	errorCode              string
}

// 测试桩仿照生产门禁，仅保留参数长度与摘要。
type hookAuditFake struct {
	mu                    sync.Mutex
	beginErr, completeErr error
	beginCount            int
	completeCount         int
	nextID                int
	records               []hookAuditRecord
	recordByID            map[string]int
}

func (f *hookAuditFake) BeginExecution(_ context.Context, intent protocol.ExecutionAuditIntent) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginCount++
	if f.beginErr != nil {
		return "", f.beginErr
	}
	digest := sha256.Sum256(intent.Args)
	f.nextID++
	id := fmt.Sprintf("hook-execution-%d", f.nextID)
	f.records = append(f.records, hookAuditRecord{
		id: id, tool: intent.Tool, source: intent.Source, argsHash: fmt.Sprintf("%x", digest), argsLength: len(intent.Args),
	})
	if f.recordByID == nil {
		f.recordByID = make(map[string]int)
	}
	f.recordByID[id] = len(f.records) - 1
	return id, nil
}

func (f *hookAuditFake) CompleteExecution(_ context.Context, id string, success bool, errorCode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCount++
	if index, ok := f.recordByID[id]; ok {
		f.records[index].success = success
		f.records[index].errorCode = errorCode
	}
	return f.completeErr
}

type panicHookWrapper struct{}

func (panicHookWrapper) WrapArgv(context.Context, protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	panic("private panic payload")
}

type countedHookMCP struct{ calls int }

func (m *countedHookMCP) CallHookTool(context.Context, string, string, map[string]any) (string, error) {
	m.calls++
	return "unexpected call", nil
}

func hookCommand(handler string, timeoutSec int, event Event, cwd string) (boundHandler, Input) {
	return boundHandler{
		Handler: Handler{Type: TypeCommand, Command: handler, TimeoutSec: timeoutSec},
		Source:  &Source{Key: "plugin:private/path/hooks.json", Scope: ScopePlugin, PluginRoot: cwd, Options: map[string]string{"secret": "do-not-persist"}},
	}, Input{HookEventName: event, Cwd: cwd}
}

func TestRunCommandAuditFailsClosedBeforePreToolUse(t *testing.T) {
	root := t.TempDir()
	marker := root + "/must-not-run"
	bh, in := hookCommand("touch "+marker, 0, EventPreToolUse, root)
	r := NewRunner(Deps{Wrapper: passthroughWrapper{}})

	got := r.runOne(context.Background(), bh, in, []byte(`{"prompt":"private input"}`))
	if got.ExitCode != 2 || !strings.Contains(string(got.Stderr), "audit unavailable") {
		t.Fatalf("missing auditor must block a pre-tool hook: %+v", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command ran without an audit intent, stat err=%v", err)
	}
}

func TestPhysicalHookTypesDoNotCallOutWhenIntentFails(t *testing.T) {
	root := t.TempDir()
	marker := root + "/command-ran"
	var httpCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpCalls++
		_, _ = w.Write([]byte("called"))
	}))
	defer server.Close()
	mcp := &countedHookMCP{}

	for _, tc := range []struct {
		name    string
		handler Handler
	}{
		{name: "command", handler: Handler{Type: TypeCommand, Command: "touch " + marker}},
		{name: "http", handler: Handler{Type: TypeHTTP, URL: server.URL}},
		{name: "mcp", handler: Handler{Type: TypeMCPTool, Server: "s", Tool: "t", Input: map[string]any{"value": "${prompt}"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &hookAuditFake{beginErr: apperr.New(apperr.CodeStorageUnavailable, "private audit failure")}
			r := NewRunner(Deps{Wrapper: passthroughWrapper{}, HTTPClient: network.NewLoopbackSafeHTTPClient(config.M11PolicyThresholds{}), MCP: mcp, Auditor: fake})
			bh := boundHandler{Handler: tc.handler, Source: &Source{Key: "plugin:private/hooks.json", Scope: ScopePlugin}}
			got := r.runOne(context.Background(), bh, Input{HookEventName: EventPreToolUse}, []byte(`{"prompt":"private input"}`))
			if got.ExitCode != 2 || fake.beginCount != 1 || fake.completeCount != 0 {
				t.Fatalf("intent failure must block before external execution: result=%+v audit=%+v", got, fake)
			}
		})
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command ran without an intent, stat err=%v", err)
	}
	if httpCalls != 0 || mcp.calls != 0 {
		t.Fatalf("external hooks ran without an intent: http=%d mcp=%d", httpCalls, mcp.calls)
	}
}

func TestRunCommandAuditIntentAndTerminalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		timeout       int
		cancel        bool
		panicWrapper  bool
		wantCode      string
		wantSuccess   bool
	}{
		{name: "success", command: "printf hook-output", wantSuccess: true},
		{name: "nonzero", command: "echo private-stderr >&2; exit 7", wantCode: string(apperr.CodeInternal)},
		{name: "timeout", command: "sleep 3", timeout: 1, wantCode: string(apperr.CodeTimeout)},
		{name: "cancel", command: "sleep 3", cancel: true, wantCode: string(apperr.CodeCancelled)},
		{name: "runner panic", command: "true", panicWrapper: true, wantCode: string(apperr.CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bh, in := hookCommand(tc.command, tc.timeout, EventPostToolUse, root)
			fake := &hookAuditFake{}
			wrapper := sandbox.ArgvWrapper(passthroughWrapper{})
			if tc.panicWrapper {
				wrapper = panicHookWrapper{}
			}
			r := NewRunner(Deps{Wrapper: wrapper, Auditor: fake})
			ctx := context.Background()
			if tc.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			stdin := []byte(`{"prompt":"private input"}`)
			got := r.runOne(ctx, bh, in, stdin)
			if fake.beginCount != 1 || fake.completeCount != 1 {
				t.Fatalf("every physical execution attempt needs one intent and completion: begin=%d complete=%d", fake.beginCount, fake.completeCount)
			}
			if len(fake.records) != 1 || fake.records[0].tool != "hook:command:PostToolUse" || fake.records[0].argsLength == 0 || fake.records[0].argsHash == "" {
				t.Fatalf("audit summary missing: %+v", fake.records)
			}
			if strings.Contains(fmt.Sprintf("%+v", fake.records), tc.command) || strings.Contains(fmt.Sprintf("%+v", fake.records), string(stdin)) || strings.Contains(fmt.Sprintf("%+v", fake.records), "do-not-persist") {
				t.Fatalf("audit record retained command, stdin or option value: %+v", fake.records)
			}
			if fake.records[0].success != tc.wantSuccess || fake.records[0].errorCode != tc.wantCode {
				t.Fatalf("wrong terminal audit result: %+v", fake.records[0])
			}
			if tc.name == "success" && (got.Err != nil || got.ExitCode != 0 || string(got.Stdout) != "hook-output") {
				t.Fatalf("successful hook result changed: %+v", got)
			}
		})
	}
}

func TestRunCommandAuditBeginFailureDoesNotRunAndCompletionFailurePreservesResult(t *testing.T) {
	root := t.TempDir()
	marker := root + "/must-not-run"
	bh, in := hookCommand("touch "+marker, 0, EventPreToolUse, root)
	fake := &hookAuditFake{beginErr: apperr.New(apperr.CodeStorageUnavailable, "private database detail")}
	r := NewRunner(Deps{Wrapper: passthroughWrapper{}, Auditor: fake})
	got := r.runOne(context.Background(), bh, in, nil)
	if got.ExitCode != 2 || fake.completeCount != 0 {
		t.Fatalf("failed intent write must fail closed before execution: result=%+v audit=%+v", got, fake)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command ran after intent write failure, stat err=%v", err)
	}

	bh, in = hookCommand("printf preserved", 0, EventPostToolUse, root)
	fake = &hookAuditFake{completeErr: errors.New("private database detail")}
	r = NewRunner(Deps{Wrapper: passthroughWrapper{}, Auditor: fake})
	got = r.runOne(context.Background(), bh, in, nil)
	if got.Err != nil || got.ExitCode != 0 || string(got.Stdout) != "preserved" || fake.completeCount != 1 {
		t.Fatalf("completion persistence failure must preserve real command result: result=%+v audit=%+v", got, fake)
	}
}

func TestHookAuditTimeoutIsTerminalWithoutWaitingBeyondOneSecond(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a bounded subprocess timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	root := t.TempDir()
	bh, in := hookCommand("sleep 3", 1, EventPostToolUse, root)
	fake := &hookAuditFake{}
	r := NewRunner(Deps{Wrapper: passthroughWrapper{}, Auditor: fake})
	got := r.runOne(ctx, bh, in, nil)
	if fake.completeCount != 1 || fake.records[0].errorCode != string(apperr.CodeTimeout) {
		t.Fatalf("timed out command needs a terminal timeout record: %+v", fake)
	}
	if !apperr.IsCode(got.Err, apperr.CodeTimeout) {
		t.Fatalf("timeout should retain typed hook error: %+v", got)
	}
}
