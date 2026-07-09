package mcpadmin

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockPrefsRepo struct{}

func (m mockPrefsRepo) GetPermissionMode(ctx context.Context) (types.PermissionMode, error) {
	return types.ModeAutoReview, nil
}
func (m mockPrefsRepo) SetPermissionMode(ctx context.Context, mode types.PermissionMode) error {
	return nil
}
func (m mockPrefsRepo) GetMaxBudget(ctx context.Context) (float64, error)           { return 0, nil }
func (m mockPrefsRepo) SetMaxBudget(ctx context.Context, limit float64) error       { return nil }
func (m mockPrefsRepo) GetActiveLLMProvider(ctx context.Context) (string, error)    { return "", nil }
func (m mockPrefsRepo) SetActiveLLMProvider(ctx context.Context, prov string) error { return nil }
func (m mockPrefsRepo) GetSystemPromptVersion(ctx context.Context) (int, error)     { return 0, nil }

type mockPolicyGate struct{}

func (m mockPolicyGate) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}
func (m mockPolicyGate) AnalyzeHook(ctx context.Context, script string) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}
func (m mockPolicyGate) TaintData(ctx context.Context, data []byte, source string, trustLevel int) ([]byte, error) {
	return data, nil
}
func (m mockPolicyGate) IsAuthorized(ctx context.Context, principal string, action string, resource string, reqCtx map[string]any) (bool, error) {
	return true, nil
}

func TestHandleMCPServers(t *testing.T) {
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
			display_name TEXT
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &MCPAdmin{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db)}
	h.InstallMgr = marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, mockPolicyGate{}, mockPrefsRepo{}, nil, nil)

	// List
	req := httptest.NewRequest("GET", "/api/v1/mcp_servers", nil)
	w := httptest.NewRecorder()
	h.HandleListMCPServers(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list mcp servers failed: %v", w.Body.String())
	}

	// Create
	body := `{"name": "test-mcp", "command": "echo", "args": ["hello"], "env": {"TEST": "1"}}`
	req = httptest.NewRequest("POST", "/api/v1/mcp_servers", bytes.NewBufferString(body))
	w = httptest.NewRecorder()
	h.HandleCreateMCPServer(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Errorf("create mcp server failed: %v", w.Body.String())
	}

	// Get (Not implemented directly? update requires path value)
}
