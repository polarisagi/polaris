package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/store"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func TestDecisionLogger(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "polaris-decisionlog-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "polaris.db")
	testStore, err := store.OpenSQLite(dbPath, schema.FS)
	if err != nil {
		t.Fatalf("store.OpenSQLite failed: %v", err)
	}
	defer testStore.Close()

	writer := store.NewDatabaseWriter(testStore.DB(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writer.Run(ctx)
	defer writer.Close()

	logger := NewSQLiteDecisionLog(writer)

	ctxJSON, _ := json.Marshal(map[string]any{"task": "test"})
	altJSON, _ := json.Marshal([]string{"optionA", "optionB"})

	entry := &types.DecisionLogEntry{
		SessionID:    "sess-001",
		AgentID:      "agent-007",
		DecisionType: "route_model",
		Context:      ctxJSON,
		Choice:       "deepseek-v4-flash",
		Alternatives: altJSON,
		Reason:       "best balance",
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	if err := logger.AppendDecision(ctx2, entry); err != nil {
		t.Fatalf("AppendDecision failed: %v", err)
	}

	var count int
	if err := testStore.DB().QueryRow(
		"SELECT COUNT(*) FROM decision_log WHERE session_id = ? AND decision_type = ?",
		"sess-001", "route_model",
	).Scan(&count); err != nil {
		t.Fatalf("QueryRow failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 decision log entry, got %d", count)
	}
}
