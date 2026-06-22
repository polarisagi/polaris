package agents

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestGovernanceAgent_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			idempotency_key TEXT UNIQUE,
			target_engine TEXT,
			operation TEXT,
			scope TEXT,
			payload BLOB,
			status TEXT,
			created_at INTEGER
		)
	`)
	if err != nil {
		t.Fatal(err)
	}

	ga, _ := NewGovernanceAgent(nil, db)

	// Check non-existent
	payload, ok := ga.CheckIdempotent(context.Background(), "hash1")
	if ok || payload != nil {
		t.Errorf("expected false, nil for non-existent key")
	}

	// Record execution
	err = ga.RecordExecution(context.Background(), "hash1", []byte("result"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check existing
	payload, ok = ga.CheckIdempotent(context.Background(), "hash1")
	if !ok || string(payload) != "result" {
		t.Errorf("expected true, 'result' for existing key, got %v, %v", ok, string(payload))
	}
}

func TestGovernanceAgent_ProbeMemory(t *testing.T) {
	ga, pressure := NewGovernanceAgent(nil, nil)

	// Direct call to Linux fallback mock logic
	freePct := probeMemoryFallback()
	if freePct < 0 {
		t.Errorf("expected positive free percentage")
	}

	// Ensure atomic update doesn't crash
	ga.probeMemory()
	val := pressure.Load()
	if val < 0 || val > 2 {
		t.Errorf("expected valid pressure level, got %d", val)
	}
}

func TestGovernanceAgent_Run(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)
	ga.probeInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go ga.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	cancel() // should exit loop gracefully
}

type mockHITLGov struct{}

func (m *mockHITLGov) Prompt(ctx context.Context, p types.HITLPrompt) (*types.HITLResponse, error) {
	return nil, nil
}
func (m *mockHITLGov) Pending(ctx context.Context) ([]types.HITLPrompt, error) { return nil, nil }
func (m *mockHITLGov) Respond(ctx context.Context, id string, response types.HITLResponse) error {
	return nil
}

func mockLLMGov(ctx context.Context, prompt string, opts ...types.InferOption) (string, error) {
	return "", nil
}

func TestGovernanceAgent_ValidateCodeWithAudit(t *testing.T) {
	ga, _ := NewGovernanceAgent(nil, nil)

	err := ga.ValidateCodeWithAudit(context.Background(), "python", []byte("print('hi')"), nil, "task1", "agent1")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	// with AST error
	err = ga.ValidateCodeWithAudit(context.Background(), "python", []byte("import subprocess"), nil, "task1", "agent1")
	if err == nil {
		t.Errorf("expected error, got nil")
	}

	// with security agent async trigger
	hitlMock := &mockHITLGov{}
	auditAgent := NewSecurityAuditAgent(mockLLMGov, hitlMock, 0, "en")
	ga.WithSecurityAuditAgent(auditAgent)

	err = ga.ValidateCodeWithAudit(context.Background(), "bash", []byte("echo hi"), nil, "task1", "agent1")
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}

	// async execution should pass without error on main thread
}
