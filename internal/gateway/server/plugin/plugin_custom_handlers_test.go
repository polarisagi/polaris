package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/store/repo"
)

func TestPluginCustomHandlers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS skills (
			name TEXT PRIMARY KEY,
			description TEXT,
			prompt TEXT,
			trust_tier INTEGER,
			plugin_id TEXT,
			catalog_id TEXT,
			deprecated BOOLEAN,
			status TEXT,
			install_path TEXT,
			error_msg TEXT,
			config TEXT,
			runtime_id TEXT,
			version TEXT,
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
			install_path TEXT,
			error_msg TEXT,
			config TEXT,
			runtime_id TEXT,
			plugin_id TEXT,
			created_at DATETIME,
			updated_at DATETIME
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{
		DB:                   db,
		ExtRepo:              repo.NewSQLiteExtensionRepository(db),
		InstallMgr:           marketplace.NewManager(repo.NewSQLiteExtensionRepository(db), nil, &dummyPolicyGate{}, repo.NewSQLiteSystemRepository(db), nil, nil, nil),
		ClearToolSchemaCache: func() {},
	}

	// 没有来源：拒绝（此前只写实例行、永远停在 installing）。
	req := httptest.NewRequest("POST", "/v1/skills/create", bytes.NewBufferString(`{"name":"test-skill"}`))
	w := httptest.NewRecorder()
	h.HandleCreateSkill(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("skill without source must be rejected: %d %s", w.Code, w.Body.String())
	}

	stub := &recordingSourceInstaller{done: make(chan marketplace.SourceInstallRequest, 2)}
	h.Catalog = stub
	req = httptest.NewRequest("POST", "/v1/skills/create", bytes.NewBufferString(`{"source":"https://github.com/openai/skills/tree/main/skills/.curated/pdf"}`))
	w = httptest.NewRecorder()
	h.HandleCreateSkill(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create skill: %d %s", w.Code, w.Body.String())
	}
	if got := <-stub.done; got.ExtType != "skill" || got.Name != "pdf" || got.Source.Path != "skills/.curated/pdf" {
		t.Fatalf("skill source install: %+v", got)
	}

	req = httptest.NewRequest("POST", "/v1/plugins/create", bytes.NewBufferString(`{"name":"fmt","source":"org/fmt@v2"}`))
	w = httptest.NewRecorder()
	h.HandleCreatePlugin(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create plugin: %d %s", w.Code, w.Body.String())
	}
	if got := <-stub.done; got.ExtType != "plugin" || got.Source.Repo != "org/fmt" || got.Source.Ref != "v2" {
		t.Fatalf("plugin source install: %+v", got)
	}
}

type recordingSourceInstaller struct {
	done chan marketplace.SourceInstallRequest
}

func (r *recordingSourceInstaller) Install(context.Context, marketplace.CatalogInstallRequest) (string, error) {
	return "", nil
}
func (r *recordingSourceInstaller) Upgrade(context.Context, marketplace.CatalogInstallRequest) error {
	return nil
}
func (r *recordingSourceInstaller) Prune(context.Context) ([]string, error) {
	return []string{"org/m/lib"}, nil
}
func (r *recordingSourceInstaller) InstallFromSource(_ context.Context, req marketplace.SourceInstallRequest) error {
	r.done <- req
	return nil
}

func TestHandlePrunePlugins(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	w := httptest.NewRecorder()
	h.HandlePrunePlugins(w, httptest.NewRequest("POST", "/v1/plugins/prune", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", w.Code)
	}
	h.Catalog = &recordingSourceInstaller{}
	w = httptest.NewRecorder()
	h.HandlePrunePlugins(w, httptest.NewRequest("POST", "/v1/plugins/prune", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "org/m/lib") {
		t.Fatalf("prune: %d %s", w.Code, w.Body.String())
	}
}
