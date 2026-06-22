package native

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/store/repo"
)

type mockCognitiveSearcher struct {
	results []ScoredResult
	err     error
}

func (m *mockCognitiveSearcher) FTSSearch(query string, limit int) ([]ScoredResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

func (m *mockCognitiveSearcher) GraphSpreadingActivation(query []string, depth int, a, b float64, c int) ([]ScoredResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

func (m *mockCognitiveSearcher) VecKNN(query []float32, k int) ([]ScoredResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

func TestExtensionActivator_FindAndActivate(t *testing.T) {
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
			created_at TEXT,
			updated_at TEXT
		);
		INSERT INTO extension_instances (id, ext_type, runtime_id, config, status, origin, catalog_id, name, publisher, trust_tier, install_path, error_msg) VALUES 
			('ext_1', 'skill', 'my_skill', '{}', 'installed', '', '', '', '', 0, '', ''),
			('ext_2', 'mcp', 'my_mcp', '{}', 'installed', '', '', '', '', 0, '', ''),
			('ext_3', 'plugin', 'my_plugin', '{}', 'installed', '', '', '', '', 0, '', ''),
			('ext_4', 'unknown', 'unk', '{}', 'installed', '', '', '', '', 0, '', '');
		
		INSERT INTO mcp_servers (id, command, args, url, transport, name, env, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, created_at, updated_at) VALUES 
			('my_mcp', 'echo', '["hello"]', '', 'stdio', '', '', 1, 0, 0, '', '', '', '', ''),
			('my_plugin', '', '', 'http://localhost:8080/sse', 'sse', '', '', 1, 0, 0, '', '', '', '', '');
	`)
	if err != nil {
		t.Fatal(err)
	}

	mcpMgr := mcp.NewMCPManager(nil, http.DefaultClient, nil)
	cog := &mockCognitiveSearcher{
		results: []ScoredResult{
			{ID: "ext_ext_1", Score: 0.9},
			{ID: "ext_ext_2", Score: 0.8},
			{ID: "ext_ext_3", Score: 0.7},
			{ID: "ext_ext_4", Score: 0.6},
			{ID: "ext_ext_missing", Score: 0.5},
		},
	}
	activator := NewExtensionActivator(repo.NewSQLiteExtensionRepository(db), cog, mcpMgr)

	hints, err := activator.FindAndActivate(context.Background(), "test goal")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if len(hints) != 1 {
		t.Errorf("expected 1 hint (skill), got %d", len(hints))
	}

	// Ensure nil cog handles gracefully
	nilActivator := NewExtensionActivator(repo.NewSQLiteExtensionRepository(db), nil, mcpMgr)
	hintsNil, _ := nilActivator.FindAndActivate(context.Background(), "test")
	if len(hintsNil) != 0 {
		t.Errorf("expected 0 hints with nil cognitive")
	}

	// Ensure empty goal handles gracefully
	hintsEmpty, _ := activator.FindAndActivate(context.Background(), "")
	if len(hintsEmpty) != 0 {
		t.Errorf("expected 0 hints with empty goal")
	}
}

func TestExtensionActivator_ActivateMCP_NilMgr(t *testing.T) {
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
		INSERT INTO extension_instances (id, ext_type, runtime_id, config, status, origin, catalog_id, name, publisher, trust_tier, install_path, error_msg) VALUES 
			('ext_2', 'mcp', 'my_mcp', '{}', 'installed', '', '', '', '', 0, '', '');
	`)
	if err != nil {
		t.Fatal(err)
	}

	activator := NewExtensionActivator(repo.NewSQLiteExtensionRepository(db), nil, nil) // mcpMgr = nil
	hint, err := activator.activateOne(context.Background(), "ext_2", "snippet")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if hint != nil {
		t.Errorf("expected nil hint when mcpMgr is nil")
	}
}
