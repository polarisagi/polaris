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
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
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
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT,
			role TEXT,
			content TEXT,
			reasoning_content TEXT NOT NULL DEFAULT '',
			tool_calls TEXT NOT NULL DEFAULT '',
			file_offset INTEGER NOT NULL DEFAULT 0,
			file_length INTEGER NOT NULL DEFAULT 0,
			dedupe_key TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			metadata TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &ChatHandler{DataDir: t.TempDir(),
		ProviderRepo:       repo.NewSQLiteProviderRepository(db),
		PersistenceService: &ChatPersistenceService{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db)},
	}

	ctx := context.Background()

	// ensureSession
	h.PersistenceService.EnsureSession(ctx, "sess-1")

	// saveMessage
	h.PersistenceService.SaveMessage(ctx, "sess-1", "user", "hello", "", "", 0)

	// saveMessage with tool calls
	h.PersistenceService.SaveMessage(ctx, "sess-1", "assistant", "", `{"type":"tool_call"}`, "", 100)

	// loadMessages
	msgs, _ := h.PersistenceService.ListMessages(ctx, "sess-1")
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages, got %d", len(msgs))
	}

	// updateSessionTitle
	h.PersistenceService.UpdateSessionTitle(ctx, "sess-1", "new title")

	// touchSession
	_ = h.PersistenceService.TouchSession(ctx, "sess-1")
}
