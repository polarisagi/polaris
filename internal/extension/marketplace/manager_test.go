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
	"github.com/polarisagi/polaris/internal/store/repo"
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

	_, err = db.Exec(`
		CREATE TABLE extension_instances (
			id TEXT PRIMARY KEY,
			ext_type TEXT,
			origin TEXT,
			catalog_id TEXT,
			name TEXT,
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
	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, pr, nil, map[string]int{"trusted": 4})

	ctx := context.Background()
	req := InstallRequest{Publisher: "trusted", TrustTier: 1}

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
	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, pg, nil, nil, nil)

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

func TestManager_InstallExtension(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	pg := &mockPolicyGate{allowed: true}
	pr := &mockPrefs{}
	inst := &mockInstaller{dir: "/test/dir"}
	extRepo := repo.NewSQLiteExtensionRepository(db)
	fsm := lifecycle.NewInstallFSM(extRepo)
	mgr := NewManager(extRepo, nil, pg, pr, nil, nil).
		WithInstaller(inst).
		WithInstallFSM(fsm)

	ctx := context.Background()
	req := InstallRequest{
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
	pr := &mockPrefs{}
	mgr := NewManager(extRepo, nil, pg, pr, nil, nil).
		WithInstallFSM(fsm)

	ctx := context.Background()
	req := InstallRequest{
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

func TestManager_UninstallExtension(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), &mockRemover{}, nil, nil, nil, nil)
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

	// Verify deletions
	var count int
	db.QueryRow("SELECT COUNT(*) FROM extension_instances").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 instances, got %d", count)
	}
	db.QueryRow("SELECT COUNT(*) FROM extension_catalog").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 catalog items, got %d", count)
	}
	db.QueryRow("SELECT COUNT(*) FROM mcp_servers").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 mcp servers, got %d", count)
	}
}

func TestManager_UpdateInstance(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	mgr := NewManager(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, nil, nil)
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
