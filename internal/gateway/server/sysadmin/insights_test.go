package sysadmin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestHandleInsights(t *testing.T) {
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

	// Insert data
	db.Exec("INSERT INTO chat_sessions (id, title, created_at, updated_at) VALUES ('s1', 'session 1', datetime('now'), datetime('now'))")
	db.Exec("INSERT INTO chat_sessions (id, title, created_at, updated_at) VALUES ('s2', 'session 2', datetime('now', '-40 days'), datetime('now', '-40 days'))")

	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m1', 's1', 'user', 'hi', datetime('now'))")
	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m2', 's1', 'assistant', 'hello', datetime('now'))")
	db.Exec("INSERT INTO chat_messages (id, session_id, role, content, created_at) VALUES ('m3', 's2', 'user', 'old', datetime('now', '-40 days'))")

	req := httptest.NewRequest("GET", "/v1/insights?days=30", nil)
	w := httptest.NewRecorder()
	h.HandleInsights(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if int(resp["total_sessions"].(float64)) != 2 {
		t.Errorf("expected 2 total sessions, got %v", resp["total_sessions"])
	}
	if int(resp["period_sessions"].(float64)) != 1 {
		t.Errorf("expected 1 period session, got %v", resp["period_sessions"])
	}
	if int(resp["total_messages"].(float64)) != 3 {
		t.Errorf("expected 3 total messages, got %v", resp["total_messages"])
	}
	if int(resp["period_messages"].(float64)) != 2 {
		t.Errorf("expected 2 period messages, got %v", resp["period_messages"])
	}

	roles := resp["role_breakdown"].(map[string]any)
	if int(roles["user"].(float64)) != 2 || int(roles["assistant"].(float64)) != 1 {
		t.Errorf("bad role breakdown: %v", roles)
	}
}
