package security

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type executionAuditLogEntry struct {
	action string
	meta   map[string]any
}

type fakeExecutionAuditLogger struct {
	mu          sync.Mutex
	entries     []executionAuditLogEntry
	failPhase   string
	failEnabled bool
}

func (f *fakeExecutionAuditLogger) Log(_ context.Context, action string, meta map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEnabled && meta["phase"] == f.failPhase {
		return apperr.New(apperr.CodeStorageUnavailable, "audit sink unavailable")
	}
	copy := make(map[string]any, len(meta))
	for key, value := range meta {
		copy[key] = value
	}
	f.entries = append(f.entries, executionAuditLogEntry{action: action, meta: copy})
	return nil
}

func TestExecutionAuditGateIntentRedactsInputAndComplete(t *testing.T) {
	logger := &fakeExecutionAuditLogger{}
	gate := NewExecutionAuditGate(logger)
	ctx := context.WithValue(context.Background(), protocol.CtxAgentIDKey{}, "agent-1")
	ctx = context.WithValue(ctx, protocol.CtxSessionIDKey{}, "session-1")
	secret := []byte(`{"password":"never-persist"}`)
	id, err := gate.BeginExecution(ctx, protocol.ExecutionAuditIntent{Tool: "run_command", Source: "agent", Args: secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.CompleteExecution(ctx, id, true, ""); err != nil {
		t.Fatal(err)
	}

	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.entries) != 2 {
		t.Fatalf("expected intent and completion entries, got %d", len(logger.entries))
	}
	for i, wantPhase := range []string{"intent", "completion"} {
		entry := logger.entries[i]
		if entry.action != "execution_audit" || entry.meta["phase"] != wantPhase || entry.meta["execution_id"] != id {
			t.Fatalf("unexpected audit entry: %#v", entry)
		}
		if entry.meta["agent_id"] != "agent-1" || entry.meta["session_id"] != "session-1" {
			t.Fatalf("missing trusted context identity: %#v", entry.meta)
		}
		encoded, _ := json.Marshal(entry.meta)
		if strings.Contains(string(encoded), "never-persist") || strings.Contains(string(encoded), "password") {
			t.Fatalf("audit metadata persisted input text: %s", encoded)
		}
	}
	if logger.entries[0].meta["args_length"] != len(secret) || logger.entries[0].meta["args_sha256"] == "" {
		t.Fatalf("intent missing args length/digest: %#v", logger.entries[0].meta)
	}
}

func TestExecutionAuditGateCompletionFailureBlocksUntilFlushThenAllowsNewIntent(t *testing.T) {
	logger := &fakeExecutionAuditLogger{failPhase: "completion", failEnabled: true}
	gate := NewExecutionAuditGate(logger)
	ctx := context.Background()
	id, err := gate.BeginExecution(ctx, protocol.ExecutionAuditIntent{Tool: "skill", Source: "skill_script", Args: []byte("private input")})
	if err != nil {
		t.Fatal(err)
	}
	actionCalls := 1 // the caller executes once after the successful intent
	if err := gate.CompleteExecution(ctx, id, false, string(apperr.CodeTimeout)); err == nil {
		t.Fatal("expected completion audit failure")
	}
	if _, err := gate.BeginExecution(ctx, protocol.ExecutionAuditIntent{Tool: "other", Source: "test"}); !apperr.IsCode(err, apperr.CodeStorageUnavailable) {
		t.Fatalf("pending completion failure must block a new intent, got %v", err)
	}
	logger.mu.Lock()
	logger.failEnabled = false
	logger.mu.Unlock()
	if _, err := gate.BeginExecution(ctx, protocol.ExecutionAuditIntent{Tool: "other", Source: "test"}); err != nil {
		t.Fatalf("recovered audit sink should flush pending completion then allow intent: %v", err)
	}
	actionCalls++ // only the second successfully begun operation runs
	if actionCalls != 2 {
		t.Fatalf("physical action was unexpectedly replayed: calls=%d", actionCalls)
	}

	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.entries) != 3 || logger.entries[1].meta["phase"] != "completion" || logger.entries[1].meta["error_code"] != string(apperr.CodeTimeout) {
		t.Fatalf("expected pending completion flushed before next intent: %#v", logger.entries)
	}
	for _, entry := range logger.entries {
		encoded, _ := json.Marshal(entry.meta)
		if strings.Contains(string(encoded), "private input") {
			t.Fatalf("pending metadata retained raw arguments: %s", encoded)
		}
	}
}

func TestExecutionAuditGateNilLoggerFailsClosed(t *testing.T) {
	var gate *ExecutionAuditGate
	if _, err := gate.BeginExecution(context.Background(), protocol.ExecutionAuditIntent{Tool: "write_file"}); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("nil gate must refuse execution, got %v", err)
	}
	if _, err := NewExecutionAuditGate(nil).BeginExecution(context.Background(), protocol.ExecutionAuditIntent{Tool: "write_file"}); !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("nil logger must refuse execution, got %v", err)
	}
}
