package native

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestExtensionManager_SearchExtension(t *testing.T) {
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
		CREATE TABLE IF NOT EXISTS extension_catalog (
			id TEXT PRIMARY KEY,
			marketplace_id TEXT,
			type TEXT,
			name TEXT,
			description TEXT,
			publisher TEXT,
			trust_tier INTEGER,
			url TEXT,
			payload TEXT,
			updated_at TEXT
		);
		INSERT INTO extension_instances (id, name, publisher, config, ext_type, origin, catalog_id, trust_tier, runtime_id, install_path, status, error_msg) VALUES 
			('1', 'Test 1', 'pub1', '{"description":"desc1"}', '', '', '', 0, '', '', '', ''),
			('2', 'Test 2', 'pub2', '{"description":"desc2"}', '', '', '', 0, '', '', '', '');
		
		INSERT INTO extension_catalog (id, name, description, publisher, payload, marketplace_id, type, trust_tier, url, updated_at) VALUES 
			('3', 'Test 3', 'desc3', 'pub3', '{"id":"3", "name":"Test 3"}', '', '', 0, '', '');
	`)
	if err != nil {
		t.Fatal(err)
	}

	cog := &mockCognitiveSearcher{
		results: []ScoredResult{
			{ID: "ext_1", Score: 0.9},
			{ID: "ext_missing", Score: 0.5},
		},
	}

	embedFn := func(ctx context.Context, text string) ([]float32, error) {
		return []float32{0.1, 0.2}, nil
	}

	searchFn := MakeExtensionSearchFn(repo.NewSQLiteExtensionRepository(db), nil, cog, embedFn)

	// Valid args
	reqBytes, _ := json.Marshal(searchExtensionArgs{Query: "test"})
	resBytes, err := searchFn(context.Background(), reqBytes)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var results []protocol.RegistryEntry
	_ = json.Unmarshal(resBytes, &results)
	if len(results) == 0 {
		t.Errorf("expected some results")
	}

	// Empty query
	emptyReqBytes, _ := json.Marshal(searchExtensionArgs{Query: ""})
	_, err = searchFn(context.Background(), emptyReqBytes)
	if err == nil {
		t.Errorf("expected error for empty query")
	}

	// Invalid args
	_, err = searchFn(context.Background(), []byte("not_json"))
	if err == nil {
		t.Errorf("expected error for invalid json")
	}

	// Nil backends
	emptySearchFn := MakeExtensionSearchFn(nil, nil, nil, nil)
	_, err = emptySearchFn(context.Background(), reqBytes)
	if err == nil {
		t.Errorf("expected error for no search backends")
	}

	// EmbedFn error
	errEmbedFn := func(ctx context.Context, text string) ([]float32, error) {
		return nil, errors.New("embed failed")
	}
	errSearchFn := MakeExtensionSearchFn(repo.NewSQLiteExtensionRepository(db), nil, cog, errEmbedFn)
	_, err = errSearchFn(context.Background(), reqBytes)
	if err != nil {
		t.Fatalf("should handle embed error gracefully: %v", err)
	}
}

func TestExtensionManager_searchLocalCatalog(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS extension_catalog (
			id TEXT PRIMARY KEY,
			marketplace_id TEXT,
			type TEXT,
			name TEXT,
			description TEXT,
			publisher TEXT,
			trust_tier INTEGER,
			url TEXT,
			payload TEXT,
			updated_at TEXT
		);
		INSERT INTO extension_catalog (id, name, description, publisher, payload, marketplace_id, type, trust_tier, url, updated_at) VALUES 
			('test1', 'My Tool', 'Does stuff', 'Dev', '{"id":"test1", "name":"My Tool"}', '', '', 0, '', '');
	`)

	res, err := searchLocalCatalog(context.Background(), repo.NewSQLiteExtensionRepository(db), "my tool")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(res) != 1 || res[0].ID != "test1" {
		t.Errorf("expected 1 result matching test1")
	}
}

func TestExtensionManager_findRegistryTarget(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS extension_catalog (
			id TEXT PRIMARY KEY,
			marketplace_id TEXT,
			type TEXT,
			name TEXT,
			description TEXT,
			publisher TEXT,
			trust_tier INTEGER,
			url TEXT,
			payload TEXT,
			updated_at TEXT
		);
		INSERT INTO extension_catalog (id, name, description, publisher, payload, marketplace_id, type, trust_tier, url, updated_at) VALUES 
			('test1', 'My Tool', 'Does stuff', 'Dev', '{"id":"test1", "name":"My Tool"}', '', '', 0, '', '');
	`)

	target := findRegistryTarget(context.Background(), "test1", repo.NewSQLiteExtensionRepository(db), nil)
	if target == nil || target.ID != "test1" {
		t.Errorf("expected to find test1 in local catalog")
	}

	targetNil := findRegistryTarget(context.Background(), "missing", repo.NewSQLiteExtensionRepository(db), nil)
	if targetNil != nil {
		t.Errorf("expected nil for missing target")
	}
}

type mockPolicyGate struct{}

func (m mockPolicyGate) ValidateCapability(ctx context.Context, principal string, extID string, cap types.CapabilityLevel) error {
	return nil
}
func (m mockPolicyGate) ValidateDelegation(ctx context.Context, principal string, extID string) error {
	return nil
}
func (m mockPolicyGate) ValidateSideEffects(ctx context.Context, principal string, extID string, effects []types.SideEffect) error {
	return nil
}
func (m mockPolicyGate) IsAuthorized(ctx context.Context, principal string, action string, res string, meta map[string]any) (bool, error) {
	return true, nil
}
func (m mockPolicyGate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

type mockPrefsRepo struct{}

func (m mockPrefsRepo) GetPermissionMode(ctx context.Context) (types.PermissionMode, error) {
	return types.ModeDefault, nil
}
func (m mockPrefsRepo) SetPermissionMode(ctx context.Context, mode types.PermissionMode) error {
	return nil
}

type mockOutbox struct{}

func (m mockOutbox) Write(ctx context.Context, entry protocol.OutboxEntry) error { return nil }

func TestExtensionManager_InstallExtension(t *testing.T) {
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
			status TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS extension_catalog (
			id TEXT PRIMARY KEY,
			marketplace_id TEXT,
			type TEXT,
			name TEXT,
			description TEXT,
			publisher TEXT,
			trust_tier INTEGER,
			url TEXT,
			payload TEXT,
			updated_at TEXT
		);
		INSERT INTO extension_catalog (id, name, description, publisher, payload, marketplace_id, type, trust_tier, url, updated_at) VALUES 
			('test_install', 'Tool', 'Desc', 'Pub', '{"id":"test_install", "type":"mcp", "install_steps":[]}', '', '', 0, '', '');
	`)
	if err != nil {
		t.Fatal(err)
	}

	installMgr := marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate{}, mockPrefsRepo{}, nil, nil)
	outbox := mockOutbox{}

	installFn := MakeExtensionInstallFn(repo.NewSQLiteExtensionRepository(db), nil, installMgr, nil, outbox)

	// Valid install
	reqBytes, _ := json.Marshal(installExtensionArgs{ID: "test_install"})
	resBytes, err := installFn(context.Background(), reqBytes)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(resBytes) == 0 {
		t.Errorf("expected response")
	}

	// Invalid args
	_, err = installFn(context.Background(), []byte("not_json"))
	if err == nil {
		t.Errorf("expected error for invalid json")
	}

	// Not found
	missingReqBytes, _ := json.Marshal(installExtensionArgs{ID: "missing"})
	_, err = installFn(context.Background(), missingReqBytes)
	if err == nil {
		t.Errorf("expected error for missing extension")
	}

	// Nil installMgr
	nilInstallFn := MakeExtensionInstallFn(repo.NewSQLiteExtensionRepository(db), nil, nil, nil, outbox)
	_, err = nilInstallFn(context.Background(), reqBytes)
	if err == nil {
		t.Errorf("expected error for nil installMgr")
	}
}
