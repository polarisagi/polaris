package chat

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestHandleSessionRecap(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_messages (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT    NOT NULL DEFAULT '',
			role       TEXT    NOT NULL DEFAULT '',
			content    TEXT    NOT NULL DEFAULT '',
			reasoning_content TEXT NOT NULL DEFAULT '',
			tool_calls TEXT NOT NULL DEFAULT '',
			file_offset INTEGER NOT NULL DEFAULT 0,
			file_length INTEGER NOT NULL DEFAULT 0,
			created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
			updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &ChatHandler{DataDir: t.TempDir(), DB: db, ChatRepo: repo.NewSQLiteChatRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	// Empty session
	req := httptest.NewRequest("GET", "/v1/sessions/s1/recap", nil)
	req.SetPathValue("sessionID", "s1")
	w := httptest.NewRecorder()
	h.HandleSessionRecap(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	// Insert data
	longUser := strings.Repeat("A", 100)
	longAssistant := strings.Repeat("B", 150)
	db.Exec("INSERT INTO chat_messages (session_id, role, content, created_at) VALUES ('s1', 'user', 'hi', datetime('now'))")
	db.Exec("INSERT INTO chat_messages (session_id, role, content, created_at) VALUES ('s1', 'assistant', '[上下文压缩摘要]...', datetime('now'))")
	db.Exec("INSERT INTO chat_messages (session_id, role, content, created_at) VALUES ('s1', 'user', ?, datetime('now'))", longUser)
	db.Exec("INSERT INTO chat_messages (session_id, role, content, created_at) VALUES ('s1', 'assistant', ?, datetime('now'))", longAssistant)

	req = httptest.NewRequest("GET", "/v1/sessions/s1/recap", nil)
	req.SetPathValue("sessionID", "s1")
	w = httptest.NewRecorder()
	h.HandleSessionRecap(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if int(resp["message_count"].(float64)) != 4 {
		t.Errorf("expected 4 messages")
	}
	if int(resp["user_messages"].(float64)) != 2 {
		t.Errorf("expected 2 user messages")
	}
	if int(resp["assistant_messages"].(float64)) != 2 {
		t.Errorf("expected 2 assistant messages")
	}
}
