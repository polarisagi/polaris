package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

func TestEpisodicProjectorHandler(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE episodic_events (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id          TEXT    NOT NULL DEFAULT '',
			seq                 INTEGER NOT NULL DEFAULT 0,
			timestamp           INTEGER NOT NULL DEFAULT 0,
			event_type          TEXT    NOT NULL DEFAULT '',
			source              TEXT    NOT NULL DEFAULT '',
			content             TEXT    NOT NULL DEFAULT '',
			embedding           BLOB,
			salience            REAL    NOT NULL DEFAULT 0.5,
			decay_weight        REAL    NOT NULL DEFAULT 1.0,
			occurred_at         INTEGER,
			embed_model_version TEXT    NOT NULL DEFAULT '',
			event_uuid          TEXT    NOT NULL DEFAULT '',
			cold                INTEGER NOT NULL DEFAULT 0,
			reasoning_state     TEXT    NOT NULL DEFAULT ''
		)
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	handler := EpisodicProjectorHandler(db)

	largePayload := make([]byte, 3000)
	for i := range largePayload {
		largePayload[i] = 'B'
	}
	ev := protocol.Event{
		ID:             "test-event-2",
		Type:           "execution_completed",
		TaskID:         "session-1",
		Payload:        largePayload,
		ReasoningState: []byte("this is a test reasoning trace"),
		CreatedAt:      time.Now(),
	}
	payloadBytes, _ := json.Marshal(ev)

	record := &substrate.OutboxRecord{
		ID:           1,
		TargetEngine: "episodic",
		Operation:    "project",
		Payload:      payloadBytes,
	}

	err = handler(context.Background(), record)
	if err != nil {
		t.Fatalf("handler execution failed: %v", err)
	}

	var content string
	var cold int
	var reasoningState string
	err = db.QueryRow("SELECT content, cold, reasoning_state FROM episodic_events WHERE event_uuid = ?", "test-event-2").Scan(&content, &cold, &reasoningState)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if content == "" {
		t.Error("content should not be empty")
	}
	if len(content) > maxEpisodicContent {
		t.Errorf("content too large: %d > %d", len(content), maxEpisodicContent)
	}
	if cold != 0 {
		t.Errorf("cold should be 0, got %d", cold)
	}
	if reasoningState != string(ev.ReasoningState) {
		t.Errorf("reasoning_state mismatch: expected %s, got %s", ev.ReasoningState, reasoningState)
	}
}
