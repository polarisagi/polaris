package chat

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestHandleListSessions(t *testing.T) {
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
			thrashing_index REAL,
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
			created_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS channels (
			id TEXT PRIMARY KEY,
			type TEXT,
			config TEXT,
			is_enabled BOOLEAN,
			status TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &ChatHandler{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	w := httptest.NewRecorder()
	h.HandleListSessions(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list sessions failed: %v", w.Body.String())
	}
}
