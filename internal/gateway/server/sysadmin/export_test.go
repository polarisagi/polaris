package sysadmin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestExportTrajectories(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_sessions (
			id TEXT PRIMARY KEY,
			title TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS chat_messages (
			id TEXT PRIMARY KEY,
			session_id TEXT,
			role TEXT,
			content TEXT,
			created_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db), ExtRepo: repo.NewSQLiteExtensionRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	// insert some test data
	db.Exec("INSERT INTO chat_sessions (id, title, created_at, updated_at) VALUES ('s1', 'title', datetime('now'), datetime('now'))")
	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m1', 's1', 'user', 'hello', datetime('now'))")
	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m2', 's1', 'assistant', 'world', datetime('now'))")
	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m3', 's1', 'user', 'test', datetime('now'))")
	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m4', 's1', 'assistant', 'test response', datetime('now'))")

	cases := []string{"", "sharegpt", "openai", "raw"}
	for _, format := range cases {
		url := "/v1/export/trajectories?format=" + format
		req := httptest.NewRequest("GET", url, nil)
		w := httptest.NewRecorder()
		h.HandleExportTrajectories(w, req)
		if w.Result().StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK for format %s", format)
		}
		if len(w.Body.Bytes()) == 0 {
			t.Errorf("expected body for format %s", format)
		}
		if bytes.Contains(w.Body.Bytes(), []byte("no qualifying sessions found")) {
			t.Errorf("expected qualifying sessions to be found for format %s", format)
		}
	}

	// Test empty case
	db.Exec("DELETE FROM chat_messages")
	req := httptest.NewRequest("GET", "/v1/export/trajectories", nil)
	w := httptest.NewRecorder()
	h.HandleExportTrajectories(w, req)
	if !bytes.Contains(w.Body.Bytes(), []byte("no qualifying sessions found")) {
		t.Errorf("expected no qualifying sessions")
	}
}
