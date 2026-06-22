package chat

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestSessionsInternal(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_sessions (
			id TEXT PRIMARY KEY,
			title TEXT,
			task_type TEXT,
			is_pinned BOOLEAN,
			status TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			total_cost REAL,
			system_prompt_version INTEGER,
			metadata TEXT,
			recap TEXT,
			tokens_in INTEGER,
			tokens_out INTEGER,
			task_duration_ms INTEGER
		);
		CREATE TABLE IF NOT EXISTS chat_messages (
			id TEXT PRIMARY KEY,
			session_id TEXT,
			role TEXT,
			content TEXT,
			tool_calls TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			metadata TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &ChatHandler{
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
	}

	ctx := context.Background()

	// ensureSession
	h.EnsureSession(ctx, "sess-1")

	// saveMessage
	h.SaveMessage(ctx, "sess-1", "user", "hello", "", 0)

	// saveMessage with tool calls
	h.SaveMessage(ctx, "sess-1", "assistant", "", `{"type":"tool_call"}`, 100)

	// loadMessages
	msgs, _ := h.LoadMessages(ctx, "sess-1")
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages, got %d", len(msgs))
	}

	// updateSessionTitle
	h.UpdateSessionTitle(ctx, "sess-1", "new title")

	// touchSession
	h.TouchSession(ctx, "sess-1")
}
