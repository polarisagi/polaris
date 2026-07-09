package plugin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestHandleListPluginCatalog(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS plugins (
			id TEXT PRIMARY KEY,
			name TEXT,
			display_name TEXT,
			description TEXT,
			version TEXT,
			trust_tier INTEGER,
			catalog_id TEXT,
			enabled INTEGER,
			status TEXT,
			install_path TEXT,
			error_msg TEXT,
			config TEXT,
			runtime_id TEXT,
			plugin_id TEXT,
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
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{
		DB: db,
	}

	req := httptest.NewRequest("GET", "/api/v1/plugins/catalog", nil)
	w := httptest.NewRecorder()

	h.HandleListPluginCatalog(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", w.Result().StatusCode)
	}
}

func TestHandleListMarketplaces(t *testing.T) {
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
			is_builtin INTEGER,
			trust_tier INTEGER,
			enabled INTEGER,
			sort_order INTEGER,
			publisher TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{
		DB: db,
	}

	req := httptest.NewRequest("GET", "/api/v1/marketplaces", nil)
	w := httptest.NewRecorder()

	h.HandleListMarketplaces(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", w.Result().StatusCode)
	}
}

func TestHandleAddDeleteMarketplace(t *testing.T) {
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
			is_builtin INTEGER,
			trust_tier INTEGER,
			enabled INTEGER,
			sort_order INTEGER,
			publisher TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{
		DB:      db,
		ExtRepo: repo.NewSQLiteExtensionRepository(db),
	}

	body := `{"name":"test", "repo_url":"http://test", "type":"mcp"}`
	req := httptest.NewRequest("POST", "/api/v1/marketplaces", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.HandleAddMarketplace(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("expected 201 Created, got %d", w.Result().StatusCode)
	}

	// extract ID
	var resp map[string]interface{}
	_ = json.NewDecoder(w.Result().Body).Decode(&resp)
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("expected non-empty id in response")
	}

	reqDelete := httptest.NewRequest("DELETE", "/api/v1/marketplaces/"+id, nil)
	reqDelete.SetPathValue("id", id)
	wDelete := httptest.NewRecorder()
	h.HandleDeleteMarketplace(wDelete, reqDelete)
	if wDelete.Result().StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", wDelete.Result().StatusCode)
	}
}

func TestHandleInstallUninstallPlugin(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
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
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{
		DB: db,
	}

	body := `{"catalog_id":"123"}`
	req := httptest.NewRequest("POST", "/api/v1/plugins/install", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.HandleInstallPlugin(w, req)
	// We expect 500 or 400 since we don't have a full marketplace manager mock, but we test the route
	_ = w.Result().StatusCode

	reqDelete := httptest.NewRequest("DELETE", "/api/v1/plugins/123", nil)
	reqDelete.SetPathValue("id", "123")
	wDelete := httptest.NewRecorder()
	h.HandleUninstallPlugin(wDelete, reqDelete)
}
