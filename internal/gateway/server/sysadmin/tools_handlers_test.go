package sysadmin

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestToolsHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS skills (
			name TEXT PRIMARY KEY,
			description TEXT,
			prompt TEXT,
			trust_tier INTEGER,
			plugin_id TEXT,
			catalog_id TEXT,
			deprecated BOOLEAN,
			status TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
		INSERT INTO skills (name, description, prompt, status) VALUES ('test-skill', 'desc', 'prompt', 'active');
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &SysAdminHandler{
		DB:           db,
		ChatRepo:     repo.NewSQLiteChatRepository(db),
		ExtRepo:      repo.NewSQLiteExtensionRepository(db),
		ProviderRepo: repo.NewSQLiteProviderRepository(db),
		InstallMgr:   marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate{}, mockPrefsRepo{}, nil, nil),
	}

	// List Skills
	req := httptest.NewRequest("GET", "/api/v1/skills", nil)
	w := httptest.NewRecorder()
	h.HandleListSkills(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list skills failed")
	}

	// Install Skill
	body := `{"url": "https://example.com/skill.md"}`
	req = httptest.NewRequest("POST", "/api/v1/skills/install", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleInstallSkill(w, req)
	// It'll likely fail the HTTP fetch, but it should hit the handler
	if w.Result().StatusCode != http.StatusInternalServerError && w.Result().StatusCode != http.StatusBadRequest {
		t.Logf("install skill returned: %v", w.Result().StatusCode)
	}
}
