package learning

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestConsumer_CursorPersistence(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS learning_cursors (
		stream_name TEXT PRIMARY KEY CHECK(stream_name IN ('task', 'version', 'heuristic', 'eval')),
		last_seq    INTEGER NOT NULL DEFAULT 0,
		updated_at  INTEGER NOT NULL
	)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	// Preset a cursor
	_, err = db.Exec(`INSERT INTO learning_cursors (stream_name, last_seq, updated_at) VALUES ('task', 5, 0)`)
	if err != nil {
		t.Fatalf("failed to insert preset cursor: %v", err)
	}

	taskEvents := make(chan TaskCompleteEvent, 10)
	versionEvents := make(chan VersionChangeEvent, 10)

	e := NewEngine(DefaultEngineConfig(), nil, nil, nil, taskEvents, versionEvents)
	e.SetDB(db)

	ctx, cancel := context.WithCancel(context.Background())

	// Start engine in background
	go func() {
		_ = e.Start(ctx)
	}()

	// Send a duplicate event (seq 5 <= cursor 5)
	taskEvents <- TaskCompleteEvent{Seq: 5, TaskID: "task_dup", TaskType: "test", Success: true}
	// Send a new event (seq 6)
	taskEvents <- TaskCompleteEvent{Seq: 6, TaskID: "task_new", TaskType: "test", Success: true}

	// Wait a bit for processing
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Check cursor in DB
	var lastSeq int64
	err = db.QueryRow("SELECT last_seq FROM learning_cursors WHERE stream_name = 'task'").Scan(&lastSeq)
	if err != nil {
		t.Fatalf("failed to query last_seq: %v", err)
	}

	if lastSeq != 6 {
		t.Fatalf("expected last_seq=6, got %d", lastSeq)
	}
}
