package mcpadmin

import (
	"bytes"
	"database/sql"
	"encoding/json"
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
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()
	applyOAuthDDL(t, db)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS mcp_servers (
			id TEXT PRIMARY KEY,
			name TEXT,
			transport TEXT,
			command TEXT,
			args TEXT,
			env TEXT,
			url TEXT,
			headers TEXT NOT NULL DEFAULT '{}',
			oauth TEXT NOT NULL DEFAULT '{}',
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
		INSERT INTO mcp_servers (id, name, plugin_id, transport, command, args, env, url, timeout, work_dir, trust_tier, enabled, created_at, updated_at)
		VALUES ('mcp-1', 'test-mcp', 'plug-1', 'stdio', 'echo', '[]', '{}', '', 10, '.', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &MCPAdmin{
		DB:         db,
		ExtRepo:    repo.NewSQLiteExtensionRepository(db),
		InstallMgr: marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate{}, mockPrefsRepo{}, nil, nil, nil),
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

// TestMCPServerHeaders_RoundTripThroughList 回归用例：GET /v1/mcp-servers 必须回显 headers 列
// （此前 SELECT 未包含该列，前端编辑表单无法预填，PUT 又是整列覆盖——静默编辑一次就会把已设置
// 的 headers 清空成 {}）。覆盖 create 写入、list 读出、update 覆盖三步。
func TestMCPServerHeaders_RoundTripThroughList(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	applyOAuthDDL(t, db)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS mcp_servers (
			id TEXT PRIMARY KEY, name TEXT, transport TEXT, command TEXT, args TEXT, env TEXT,
			url TEXT, headers TEXT NOT NULL DEFAULT '{}', oauth TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER, timeout INTEGER, trust_tier INTEGER, catalog_id TEXT, plugin_id TEXT,
			work_dir TEXT, requires_network INTEGER, created_at TEXT, updated_at TEXT
		);
		CREATE TABLE IF NOT EXISTS plugins (id TEXT PRIMARY KEY, name TEXT, display_name TEXT);
		CREATE TABLE IF NOT EXISTS extension_instances (
			id TEXT PRIMARY KEY, ext_type TEXT, origin TEXT, catalog_id TEXT, name TEXT,
			installed_version TEXT DEFAULT '', publisher TEXT, trust_tier INTEGER, runtime_id TEXT,
			install_path TEXT, config TEXT, status TEXT, error_msg TEXT,
			created_at TEXT DEFAULT CURRENT_TIMESTAMP, updated_at TEXT DEFAULT CURRENT_TIMESTAMP, deleted_at TEXT
		);
	`); err != nil {
		t.Fatal(err)
	}

	h := &MCPAdmin{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db),
		InstallMgr: marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate{}, mockPrefsRepo{}, nil, nil, nil)}

	create := httptest.NewRequest("POST", "/v1/mcp-servers",
		bytes.NewBufferString(`{"id":"srv-h","name":"h","transport":"streamable_http","url":"https://example.com","headers":{"X-Api-Key":"secret-1"}}`))
	w := httptest.NewRecorder()
	h.HandleCreateMCPServer(w, create)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Result().StatusCode, w.Body.String())
	}

	headersOf := func() map[string]string {
		t.Helper()
		w := httptest.NewRecorder()
		h.HandleListMCPServers(w, httptest.NewRequest("GET", "/v1/mcp-servers", nil))
		var body struct {
			MCPServers []struct {
				ID      string            `json:"id"`
				Headers map[string]string `json:"headers"`
			} `json:"mcp_servers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, s := range body.MCPServers {
			if s.ID == "srv-h" {
				return s.Headers
			}
		}
		t.Fatal("srv-h not found in list")
		return nil
	}

	if got := headersOf(); got["X-Api-Key"] != "secret-1" {
		t.Fatalf("list must echo back headers set at create, got %+v", got)
	}

	update := httptest.NewRequest("PUT", "/v1/mcp-servers/srv-h",
		bytes.NewBufferString(`{"name":"h","transport":"streamable_http","url":"https://example.com","enabled":true,"headers":{"X-Api-Key":"secret-2","X-Extra":"v"}}`))
	update.SetPathValue("serverID", "srv-h")
	w = httptest.NewRecorder()
	h.HandleUpdateMCPServer(w, update)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("update: %d %s", w.Result().StatusCode, w.Body.String())
	}
	if got := headersOf(); got["X-Api-Key"] != "secret-2" || got["X-Extra"] != "v" {
		t.Fatalf("update must overwrite headers, got %+v", got)
	}
}
