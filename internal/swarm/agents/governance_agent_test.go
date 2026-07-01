package agents

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
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
