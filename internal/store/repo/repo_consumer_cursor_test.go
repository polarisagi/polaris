package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func TestConsumerCursorRepository_LoadSaveMonotonic(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, err := schema.FS.ReadFile("002_outbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := NewSQLiteConsumerCursorRepository(db)

	if got, err := r.GetCursor(ctx, "memory_agent.whisper"); err != nil || got != 0 {
		t.Fatalf("空游标应为 0: %d %v", got, err)
	}
	if err := r.SaveCursor(ctx, "memory_agent.whisper", 42); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveCursor(ctx, "memory_agent.whisper", 10); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetCursor(ctx, "memory_agent.whisper"); got != 42 {
		t.Fatalf("游标不得回退: got %d", got)
	}
	if got, _ := r.GetCursor(ctx, "learning.task"); got != 0 {
		t.Fatalf("消费者互不影响: got %d", got)
	}
}
