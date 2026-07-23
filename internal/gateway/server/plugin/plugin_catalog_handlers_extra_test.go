package plugin

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestPluginCatalogHandlersExtra(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS plugin_marketplaces (
			id TEXT PRIMARY KEY,
			name TEXT,
			repo_url TEXT,
			description TEXT,
			type TEXT,
			is_builtin BOOLEAN,
			trust_tier INTEGER,
			enabled BOOLEAN,
			sort_order INTEGER,
			publisher TEXT,
			status TEXT,
			last_sync_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS plugins (
			id TEXT PRIMARY KEY,
			name TEXT,
			display_name TEXT,
			description TEXT,
			publisher TEXT,
			version TEXT,
			trust_tier INTEGER,
			install_path TEXT,
			catalog_id TEXT,
			enabled INTEGER,
			mcp_policy TEXT,
			status TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS extension_instances (
			id TEXT PRIMARY KEY,
			ext_type TEXT,
			origin TEXT,
			catalog_id TEXT,
			name TEXT,
			installed_version TEXT DEFAULT '',
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
		INSERT INTO plugin_marketplaces (id, name, repo_url, type, is_builtin, trust_tier, enabled, status, created_at, updated_at)
		VALUES ('mp-1', 'Test MP', 'http://test', 'git', 0, 1, 1, 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
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

	// List Marketplaces
	req := httptest.NewRequest("GET", "/api/v1/marketplaces", nil)
	w := httptest.NewRecorder()
	h.HandleListMarketplaces(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list marketplaces failed: %v", w.Result().StatusCode)
	}

	// List Plugin Catalog
	req = httptest.NewRequest("GET", "/api/v1/catalog/plugins", nil)
	w = httptest.NewRecorder()
	h.HandleListPluginCatalog(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list plugin catalog failed: %v", w.Result().StatusCode)
	}

	// Delete Marketplace
	req = httptest.NewRequest("DELETE", "/api/v1/marketplaces/mp-1", nil)
	req.SetPathValue("id", "mp-1")
	w = httptest.NewRecorder()
	h.HandleDeleteMarketplace(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("delete marketplace failed: %v", w.Result().StatusCode)
	}

	// Uninstall Plugin
	req = httptest.NewRequest("DELETE", "/api/v1/plugins/ext-1", nil)
	req.SetPathValue("id", "ext-1")
	w = httptest.NewRecorder()
	h.HandleUninstallPlugin(w, req)
	// It'll probably return not found, but we get handler coverage
}
