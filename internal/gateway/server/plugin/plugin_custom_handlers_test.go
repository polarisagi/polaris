package plugin

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

func TestPluginCustomHandlers(t *testing.T) {
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
			install_path TEXT,
			error_msg TEXT,
			config TEXT,
			runtime_id TEXT,
			version TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS extension_instances (
			id TEXT PRIMARY KEY,
			ext_type TEXT,
			origin TEXT,
			catalog_id TEXT,
			name TEXT,
			publisher TEXT,
			trust_tier INTEGER,
			runtime_id TEXT,
			install_path TEXT,
			config TEXT,
			status TEXT,
			error_msg TEXT,
			created_at TEXT DEFAULT CURRENT_TIMESTAMP,
			updated_at TEXT DEFAULT CURRENT_TIMESTAMP,
			deleted_at TEXT
		);
		CREATE TABLE IF NOT EXISTS plugins (
			id TEXT PRIMARY KEY,
			name TEXT,
			display_name TEXT,
			description TEXT,
			version TEXT,
			trust_tier INTEGER,
			catalog_id TEXT,
			enabled BOOLEAN,
			status TEXT,
			install_path TEXT,
			error_msg TEXT,
			config TEXT,
			runtime_id TEXT,
			plugin_id TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS apps (
			id TEXT PRIMARY KEY,
			name TEXT,
			display_name TEXT,
			description TEXT,
			version TEXT,
			trust_tier INTEGER,
			catalog_id TEXT,
			enabled BOOLEAN,
			url TEXT,
			publisher TEXT,
			status TEXT,
			install_path TEXT,
			error_msg TEXT,
			config TEXT,
			runtime_id TEXT,
			plugin_id TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{
		DB:                   db,
		ExtRepo:              repo.NewSQLiteExtensionRepository(db),
		InstallMgr:           marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, &dummyPolicyGate{}, repo.NewSQLiteSystemRepository(db), nil, nil),
		ClearToolSchemaCache: func() {},
	}

	// Create Skill
	body := `{"name": "test-skill", "description": "desc", "prompt": "prompt"}`
	req := httptest.NewRequest("POST", "/api/v1/skills/custom", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.HandleCreateSkill(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create skill failed: %v", w.Body.String())
	}

	// Create Plugin
	body = `{"name": "test-plugin", "display_name": "Test", "description": "desc", "version": "1.0.0"}`
	req = httptest.NewRequest("POST", "/api/v1/plugins/custom", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreatePlugin(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create plugin failed: %v", w.Body.String())
	}

	// Create App
	body = `{"name": "test-app", "display_name": "Test", "description": "desc", "version": "1.0.0"}`
	req = httptest.NewRequest("POST", "/api/v1/apps/custom", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreateApp(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create app failed: %v", w.Body.String())
	}
}
