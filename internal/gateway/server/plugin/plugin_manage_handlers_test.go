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

func TestPluginManageHandlers(t *testing.T) {
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
		INSERT INTO plugins (id, name, display_name, description, publisher, version, trust_tier, install_path, catalog_id, enabled, mcp_policy, status, created_at, updated_at)
		VALUES ('plug-1', 'test-plugin', 'Test', 'Desc', 'Polaris', '1.0', 1, '/tmp', 'cat-1', 1, '{}', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
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

	// List plugins
	req := httptest.NewRequest("GET", "/api/v1/plugins", nil)
	w := httptest.NewRecorder()
	h.HandleListPlugins(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list plugins failed, code: %d, body: %s", w.Result().StatusCode, w.Body.String())
	}

	// Update plugin
	body := `{"enabled": false}`
	req = httptest.NewRequest("PUT", "/api/v1/plugins/plug-1", bytes.NewBufferString(body))
	req.SetPathValue("id", "plug-1")
	w = httptest.NewRecorder()
	h.HandleUpdatePlugin(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("update plugin failed: %v", w.Body.String())
	}

	// Toggle MCP (Not Found since we don't have MCP manager set up but should return 404/500 not panic)
	req = httptest.NewRequest("POST", "/api/v1/plugins/plug-1/mcp/toggle", nil)
	req.SetPathValue("id", "plug-1")
	req.SetPathValue("serverName", "my-mcp")
	w = httptest.NewRecorder()
	h.HandleTogglePluginMCP(w, req)
	if w.Result().StatusCode != http.StatusInternalServerError && w.Result().StatusCode != http.StatusNotFound {
		t.Logf("toggle plugin mcp returned: %v", w.Result().StatusCode)
	}
}
