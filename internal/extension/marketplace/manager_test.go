package marketplace

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockPolicyGate struct {
	allowed bool
	reason  string
	err     error
}

func (m *mockPolicyGate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	if m.err != nil {
		return types.PolicyReviewResult{}, m.err
	}
	return types.PolicyReviewResult{Allowed: m.allowed, Reason: m.reason}, nil
}

func (m *mockPolicyGate) IsAuthorized(ctx context.Context, principal, action, resource string, contextData map[string]any) (bool, error) {
	return m.allowed, m.err
}

type mockPrefs struct{}

func (m *mockPrefs) GetPermissionMode(ctx context.Context) (types.PermissionMode, error) {
	return types.ModeAutoReview, nil
}
func (m *mockPrefs) SetPermissionMode(ctx context.Context, mode types.PermissionMode) error {
	return nil
}
func (m *mockPrefs) GetString(ctx context.Context, key string) (string, error) { return "", nil }
func (m *mockPrefs) SetString(ctx context.Context, key, val string) error      { return nil }

type mockInstaller struct {
	dir string
	err error
}

func (m *mockInstaller) Install(ctx context.Context, target any) (string, error) {
	return m.dir, m.err
}

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE extension_instances (
			id TEXT PRIMARY KEY,
			ext_type TEXT,
			origin TEXT,
			catalog_id TEXT,
			name TEXT,
			installed_version TEXT DEFAULT '',
			publisher TEXT,
			trust_tier INTEGER,
			runtime_id TEXT,
			config TEXT,
			status TEXT,
			install_path TEXT,
			error_msg TEXT,
			created_at TEXT,
			updated_at TEXT
		);
		CREATE TABLE extension_catalog (id TEXT PRIMARY KEY, marketplace_id TEXT);
		CREATE TABLE plugin_marketplaces (id TEXT PRIMARY KEY, is_builtin INTEGER);
		CREATE TABLE mcp_servers (id TEXT PRIMARY KEY, plugin_id TEXT);
		CREATE TABLE skills (name TEXT PRIMARY KEY, plugin_id TEXT);
		CREATE TABLE plugins (id TEXT PRIMARY KEY);
		CREATE TABLE apps (id TEXT PRIMARY KEY);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestManager_Authorize(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	pg := &mockPolicyGate{allowed: true}
	pr := &mockPrefs{}
	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, pr, nil, map[string]int{"trusted": 4}, nil)

	ctx := context.Background()
	req := protocol.ExtensionInstallRequest{Publisher: "trusted", TrustTier: 1}

	err := mgr.Authorize(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	pg.allowed = false
	pg.reason = "forbidden: test"
	err = mgr.Authorize(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "installation forbidden: forbidden: test") {
		t.Errorf("unexpected error: %v", err)
	}

	pg.reason = "denied by default"
	err = mgr.Authorize(ctx, req)
	if err == nil || !errors.Is(err, ErrRequiresApproval) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManager_AuthorizeAction(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	pg := &mockPolicyGate{allowed: true}
	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, nil, nil, nil, nil)

	ctx := context.Background()
	err := mgr.AuthorizeAction(ctx, "system", "manage", nil)
	if err != nil {
		t.Fatal(err)
	}

	pg.allowed = false
	err = mgr.AuthorizeAction(ctx, "system", "manage", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

type mockFSMInstaller struct {
	extType types.ExtType
	extRepo protocol.ExtensionRepository
}

func (m *mockFSMInstaller) ExtType() types.ExtType { return m.extType }
func (m *mockFSMInstaller) Install(ctx context.Context, req lifecycle.InstallReq) (string, error) {
	_ = m.extRepo.UpdateInstanceStatus(ctx, req.InstID, "installed", "")
	if req.LocalPath != "" {
		return req.LocalPath, nil
	}
	return "/test/dir", nil
}
func (m *mockFSMInstaller) Uninstall(ctx context.Context, req lifecycle.UninstallReq) error {
	return nil
}

func TestManager_InstallExtension(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	pg := &mockPolicyGate{allowed: true}
	pr := &mockPrefs{}
	inst := &mockInstaller{dir: "/test/dir"}
	extRepo := repo.NewSQLiteExtensionRepository(db)
	fsm := lifecycle.NewInstallFSM(extRepo)
	fsm.RegisterInstaller(&mockFSMInstaller{extType: types.ExtType("mcp"), extRepo: extRepo})
	mgr := NewManager(extRepo, nil, pg, pr, nil, nil, nil).
		WithInstaller(inst).
		WithInstallFSM(fsm)

	ctx := context.Background()
	req := protocol.ExtensionInstallRequest{
		ExtensionID: "ext_1",
		ExtType:     "mcp",
		CatalogID:   "cat_1",
		Target:      "dummy",
	}

	err := mgr.InstallExtension(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	var status, path string
	err = db.QueryRow("SELECT status, install_path FROM extension_instances WHERE id='ext_1'").Scan(&status, &path)
	if err != nil {
		t.Fatal(err)
	}
	if status != "installed" || path != "/test/dir" {
		t.Errorf("unexpected instance state: status=%s, path=%s", status, path)
	}
}

func TestManager_InstallExtension_LocalPath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	pg := &mockPolicyGate{allowed: true}
	extRepo := repo.NewSQLiteExtensionRepository(db)
	fsm := lifecycle.NewInstallFSM(extRepo)
	fsm.RegisterInstaller(&mockFSMInstaller{extType: types.ExtType("skill"), extRepo: extRepo})
	pr := &mockPrefs{}
	mgr := NewManager(extRepo, nil, pg, pr, nil, nil, nil).
		WithInstallFSM(fsm)

	ctx := context.Background()
	req := protocol.ExtensionInstallRequest{
		ExtensionID: "ext_local",
		ExtType:     "skill",
		LocalPath:   "/local/path",
	}

	err := mgr.InstallExtension(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	var status, path string
	err = db.QueryRow("SELECT status, install_path FROM extension_instances WHERE id='ext_local'").Scan(&status, &path)
	if err != nil {
		t.Fatal(err)
	}
	if status != "installed" || path != "/local/path" {
		t.Errorf("unexpected instance state: status=%s, path=%s", status, path)
	}
}

type mockRemover struct {
	removed string
}

func (m *mockRemover) Remove(id string) {
	m.removed = id
}

type mockOutbox struct{}

func (m *mockOutbox) Write(ctx context.Context, entry protocol.OutboxEntry) error {
	return nil
}

func TestManager_UninstallExtension(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), &mockRemover{}, nil, nil, nil, nil, &mockOutbox{})
	ctx := context.Background()

	// Setup data
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`
		INSERT INTO extension_instances (id, ext_type, origin, catalog_id, runtime_id, install_path, name, publisher, trust_tier, config, status, error_msg, created_at, updated_at)
		VALUES ('ext_1', 'mcp', 'user', 'cat_1', 'mcp_1', '', '', '', 0, '{}', '', '', ?, ?);
		INSERT INTO extension_catalog (id, marketplace_id) VALUES ('cat_1', '');
		INSERT INTO mcp_servers (id, plugin_id) VALUES ('mcp_1', '');
	`, now, now)
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.UninstallExtension(ctx, "cat_1")
	if err != nil {
		t.Fatal(err)
	}

	// Verify status updated to uninstalling (deletion is async via queue)
	var status string
	db.QueryRow("SELECT status FROM extension_instances WHERE id='ext_1'").Scan(&status)
	if status != "uninstalling" {
		t.Errorf("expected status 'uninstalling', got %s", status)
	}
}

func TestManager_UpdateInstance(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil, nil)
	ctx := context.Background()

	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`
		INSERT INTO extension_instances (id, status, error_msg, created_at, updated_at)
		VALUES ('ext_1', 'installing', 'old_err', ?, ?);
	`, now, now)
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.UpdateInstance(ctx, "ext_1", InstanceUpdate{
		Status:     "installed",
		ClearError: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var status string
	var errMsg sql.NullString
	err = db.QueryRow("SELECT status, error_msg FROM extension_instances WHERE id='ext_1'").Scan(&status, &errMsg)
	if err != nil {
		t.Fatal(err)
	}

	if status != "installed" {
		t.Errorf("expected status 'installed', got '%s'", status)
	}
	if errMsg.Valid {
		t.Errorf("expected error_msg to be NULL, got '%s'", errMsg.String)
	}
}

// TestManager_InstallExtension_PolicyGateDenied_PreservesForbidden 验证 S-06
// 修复：PolicyGate 拒绝时 InstallExtension 必须保留 CodeForbidden 语义，网关侧
// 405/403 不得因 apperr.Wrap(apperr.CodeInternal, ...) 覆盖内层 Code 而退化成 500。
// 回归锚点：修复前 apperr.CodeOf(err) 恒为 CodeInternal，本用例必失败。
func TestManager_InstallExtension_PolicyGateDenied_PreservesForbidden(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	pg := &mockPolicyGate{allowed: false, reason: "forbidden: test policy"}
	pr := &mockPrefs{}
	extRepo := repo.NewSQLiteExtensionRepository(db)
	mgr := NewManager(extRepo, nil, pg, pr, nil, nil, nil)

	ctx := context.Background()
	req := protocol.ExtensionInstallRequest{
		ExtensionID: "ext_denied",
		ExtType:     "mcp",
		CatalogID:   "cat_denied",
		Target:      "dummy",
	}

	err := mgr.InstallExtension(ctx, req)
	if err == nil {
		t.Fatal("expected error when PolicyGate denies installation, got nil")
	}
	if !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Errorf("expected CodeForbidden, got %v (err=%v)", apperr.CodeOf(err), err)
	}
	if status := apperr.HTTPStatus(apperr.CodeOf(err)); status != 403 {
		t.Errorf("expected HTTP 403, got %d", status)
	}
}
