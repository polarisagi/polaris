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

// setupUpgradeDB 创建包含 extension_instances 和 extension_catalog 的内存 DB。
func setupUpgradeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS extension_catalog (
			id TEXT PRIMARY KEY,
			name TEXT,
			display_name TEXT,
			description TEXT,
			publisher TEXT,
			ext_type TEXT,
			version TEXT DEFAULT '',
			tags TEXT,
			homepage TEXT,
			icon_url TEXT,
			status TEXT,
			release_notes TEXT,
			manifest TEXT,
			signature TEXT,
			created_at TEXT,
			updated_at TEXT
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
		CREATE TABLE IF NOT EXISTS plugins (
			id TEXT PRIMARY KEY,
			name TEXT, display_name TEXT, description TEXT, publisher TEXT,
			version TEXT, trust_tier INTEGER, install_path TEXT, catalog_id TEXT,
			enabled INTEGER, mcp_policy TEXT, status TEXT, created_at DATETIME, updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestHandleUpgradePlugin_FailureDoesNotClearInstallPath 验证：
// 当 catalog_id/version 为空（不支持在线升级）时，API 返回 400 但 install_path 不被清空（B3 验收）。
func TestHandleUpgradePlugin_FailureDoesNotClearInstallPath(t *testing.T) {
	db := setupUpgradeDB(t)
	defer db.Close()

	// 插入一个没有 catalog_id 关联的插件实例（模拟"不支持在线升级"场景）
	_, err := db.Exec(`
		INSERT INTO extension_instances (id, ext_type, origin, catalog_id, name, installed_version, install_path, status)
		VALUES ('inst-1', 'plugin', 'marketplace', '', 'my-plugin', '1.0', '/opt/polaris/plugins/my-plugin', 'installed')
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/inst-1/upgrade", bytes.NewBufferString("{}"))
	req.SetPathValue("id", "inst-1")
	w := httptest.NewRecorder()
	h.HandleUpgradePlugin(w, req)

	// 不支持在线升级应返回 400
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when no catalog_id, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 关键验收：install_path 不被清空
	var installPath string
	_ = db.QueryRow("SELECT install_path FROM extension_instances WHERE id='inst-1'").Scan(&installPath)
	if installPath == "" {
		t.Error("install_path was cleared on upgrade failure, expected it to remain '/opt/polaris/plugins/my-plugin'")
	}
	if installPath != "/opt/polaris/plugins/my-plugin" {
		t.Errorf("install_path changed unexpectedly: %q", installPath)
	}
}

// TestHandleUpgradePlugin_AlreadyUpToDate 验证已是最新版时返回 304。
func TestHandleUpgradePlugin_AlreadyUpToDate(t *testing.T) {
	db := setupUpgradeDB(t)
	defer db.Close()

	// 插入 catalog
	_, err := db.Exec(`
		INSERT INTO extension_catalog (id, name, display_name, description, publisher, ext_type, version, status)
		VALUES ('cat-1', 'my-plugin', 'My Plugin', '', 'Polaris', 'plugin', '2.0', 'active')
	`)
	if err != nil {
		t.Fatal(err)
	}
	// 插入已安装且版本与 catalog 相同
	_, err = db.Exec(`
		INSERT INTO extension_instances (id, ext_type, origin, catalog_id, name, installed_version, install_path, status)
		VALUES ('inst-2', 'plugin', 'marketplace', 'cat-1', 'my-plugin', '2.0', '/opt/polaris/plugins/my-plugin', 'installed')
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/inst-2/upgrade", bytes.NewBufferString("{}"))
	req.SetPathValue("id", "inst-2")
	w := httptest.NewRecorder()
	h.HandleUpgradePlugin(w, req)

	if w.Code != http.StatusNotModified {
		t.Errorf("expected 304 when already up to date, got %d (body: %s)", w.Code, w.Body.String())
	}

	// install_path 不应被清空
	var installPath string
	_ = db.QueryRow("SELECT install_path FROM extension_instances WHERE id='inst-2'").Scan(&installPath)
	if installPath == "" {
		t.Error("install_path was cleared on 304 path, expected it to remain")
	}
}

// TestHandleUpgradePlugin_Success 验证升级成功后版本更新且 install_path 保留。
func TestHandleUpgradePlugin_Success(t *testing.T) {
	db := setupUpgradeDB(t)
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO extension_catalog (id, name, display_name, description, publisher, ext_type, version, status)
		VALUES ('cat-2', 'my-plugin-v2', 'My Plugin V2', '', 'Polaris', 'plugin', '3.0', 'active')
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO extension_instances (id, ext_type, origin, catalog_id, name, installed_version, install_path, status)
		VALUES ('inst-3', 'plugin', 'marketplace', 'cat-2', 'my-plugin-v2', '2.5', '/opt/plugins/v2', 'installed')
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/inst-3/upgrade", bytes.NewBufferString("{}"))
	req.SetPathValue("id", "inst-3")
	w := httptest.NewRecorder()
	h.HandleUpgradePlugin(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 on upgrade, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 版本已更新
	var newVersion, installPath string
	_ = db.QueryRow("SELECT installed_version, install_path FROM extension_instances WHERE id='inst-3'").Scan(&newVersion, &installPath)
	if newVersion != "3.0" {
		t.Errorf("expected version=3.0, got %q", newVersion)
	}
	// install_path 不应被清空
	if installPath != "/opt/plugins/v2" {
		t.Errorf("install_path changed or cleared: %q", installPath)
	}
}
