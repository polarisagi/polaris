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

func TestHandlePrompts(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS prompt_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_type TEXT NOT NULL,
			content TEXT NOT NULL,
			description TEXT,
			is_active BOOLEAN NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{SoulMDContent: new(string),
		DB:        db,
		PromptMgr: prompt.NewManager(t.TempDir(), nil),
	}

	// List
	req := httptest.NewRequest("GET", "/api/v1/prompts", nil)
	w := httptest.NewRecorder()
	h.HandleListPrompts(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list prompts failed")
	}

	// Create/Set
	body := `{"value":"test prompt"}`
	req = httptest.NewRequest("POST", "/api/v1/prompts/identity", bytes.NewBufferString(body))
	req.SetPathValue("name", "identity")
	w = httptest.NewRecorder()
	h.HandleSetPrompt(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("set prompt failed")
	}
}

func TestHandleBudget(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS kv_store (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{SoulMDContent: new(string),
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		BudgetRepo:   repo.NewSQLiteBudgetRepository(db),
	}

	// Get Budget
	req := httptest.NewRequest("GET", "/api/v1/budget", nil)
	w := httptest.NewRecorder()
	h.HandleGetBudget(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("get budget failed")
	}

	// Set Budget
	body := `{"monthly_usd": 50.0}`
	req = httptest.NewRequest("POST", "/api/v1/budget", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleSetBudget(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("set budget failed")
	}
}
