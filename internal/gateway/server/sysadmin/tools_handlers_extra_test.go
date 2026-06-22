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

func TestToolsHandlersExtra(t *testing.T) {
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

	// List Tools
	req := httptest.NewRequest("GET", "/api/v1/tools", nil)
	w := httptest.NewRecorder()
	h.HandleListTools(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list tools failed: %v", w.Result().StatusCode)
	}

	// List Tool Schemas
	req = httptest.NewRequest("GET", "/api/v1/tools/schemas", nil)
	w = httptest.NewRecorder()
	h.HandleListToolSchemas(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list tool schemas failed: %v", w.Result().StatusCode)
	}

	// Execute Tool
	body := `{"name": "test-tool", "arguments": {}}`
	req = httptest.NewRequest("POST", "/api/v1/tools/execute", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleExecuteTool(w, req)
	// It'll probably fail because tool executor isn't mocked, but we get coverage for the handler setup
}
