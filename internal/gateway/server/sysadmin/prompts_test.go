package sysadmin

import (
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/store/repo"

	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestHandlePromptsBasic(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS prompt_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT,
			value TEXT,
			author TEXT,
			status TEXT,
			created_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	db.Exec("INSERT INTO prompt_versions (key, value, author, status, created_at) VALUES ('identity', 'old identity', 'system', 'active', datetime('now'))")

	h := &SysAdminHandler{SoulMDContent: new(string),
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		PromptMgr:    prompt.NewManager(t.TempDir(), nil),
	}

	// List prompts
	req := httptest.NewRequest("GET", "/v1/config/prompts", nil)
	w := httptest.NewRecorder()
	h.HandleListPrompts(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	// Get prompt
	req = httptest.NewRequest("GET", "/v1/config/prompts/identity", nil)
	req.SetPathValue("name", "identity")
	w = httptest.NewRecorder()
	h.HandleGetPrompt(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	// Get unknown prompt
	req = httptest.NewRequest("GET", "/v1/config/prompts/unknown", nil)
	req.SetPathValue("name", "unknown")
	w = httptest.NewRecorder()
	h.HandleGetPrompt(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found")
	}

	// Set prompt
	body := `{"value": "new identity"}`
	req = httptest.NewRequest("PUT", "/v1/config/prompts/identity", bytes.NewBufferString(body))
	req.SetPathValue("name", "identity")
	w = httptest.NewRecorder()
	h.HandleSetPrompt(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}

	// Reset prompt
	req = httptest.NewRequest("DELETE", "/v1/config/prompts/identity", nil)
	req.SetPathValue("name", "identity")
	w = httptest.NewRecorder()
	h.HandleResetPrompt(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK")
	}
}
