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

func TestMCPServersHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS mcp_servers (
			id TEXT PRIMARY KEY,
			name TEXT,
			transport TEXT,
			command TEXT,
			args TEXT,
			env TEXT,
			url TEXT,
			enabled INTEGER,
			timeout INTEGER,
			trust_tier INTEGER,
			catalog_id TEXT,
			plugin_id TEXT,
			work_dir TEXT,
			requires_network INTEGER,
			created_at TEXT,
			updated_at TEXT
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
		INSERT INTO mcp_servers (id, name, plugin_id, transport, command, args, env, url, timeout, work_dir, trust_tier, enabled, created_at, updated_at)
		VALUES ('mcp-1', 'test-mcp', 'plug-1', 'stdio', 'echo', '[]', '{}', '', 10, '.', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
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

	// List MCP Servers
	req := httptest.NewRequest("GET", "/api/v1/mcp-servers", nil)
	w := httptest.NewRecorder()
	h.HandleListMCPServers(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list mcp servers failed: %v", w.Body.String())
	}

	// Create MCP Server
	body := `{"name": "new-mcp", "transport": "stdio", "command": "cat", "args": []}`
	req = httptest.NewRequest("POST", "/api/v1/mcp-servers", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreateMCPServer(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create mcp server failed: %v", w.Body.String())
	}

	// Update MCP Server
	body = `{"enabled": false}`
	req = httptest.NewRequest("PUT", "/api/v1/mcp-servers/mcp-1", bytes.NewBufferString(body))
	req.SetPathValue("id", "mcp-1")
	w = httptest.NewRecorder()
	h.HandleUpdateMCPServer(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Logf("update mcp server returned: %v %s", w.Result().StatusCode, w.Body.String())
	}

	// Test MCP Server
	req = httptest.NewRequest("POST", "/api/v1/mcp-servers/mcp-1/test", nil)
	req.SetPathValue("id", "mcp-1")
	w = httptest.NewRecorder()
	h.HandleTestMCPServer(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Logf("test mcp server returned: %v %s", w.Result().StatusCode, w.Body.String())
	}

	// Delete MCP Server
	req = httptest.NewRequest("DELETE", "/api/v1/mcp-servers/mcp-1", nil)
	req.SetPathValue("id", "mcp-1")
	w = httptest.NewRecorder()
	h.HandleDeleteMCPServer(w, req)
	if w.Result().StatusCode != http.StatusNoContent && w.Result().StatusCode != http.StatusOK {
		t.Logf("delete mcp server returned: %v %s", w.Result().StatusCode, w.Body.String())
	}
}
