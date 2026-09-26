package plugin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type stubCatalogSync struct{ synced []string }

func (s *stubCatalogSync) Sync(_ context.Context, mp protocol.Marketplace, _ bool) ([]types.ExtCatalogRow, error) {
	s.synced = append(s.synced, mp.ID)
	return []types.ExtCatalogRow{{ID: mp.ID + "/x"}}, nil
}

func TestHandleSyncMarketplaces(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, ddl := range []string{
		`CREATE TABLE plugin_marketplaces (id TEXT, name TEXT, type TEXT, publisher TEXT, repo_url TEXT, description TEXT,
		  is_builtin INTEGER, trust_tier INTEGER, enabled INTEGER, sort_order INTEGER, created_at TEXT)`,
		`INSERT INTO plugin_marketplaces VALUES('org/mkt','m','plugin','org','https://github.com/org/mkt','',0,2,1,0,'')`,
		`CREATE TABLE extension_catalog (id TEXT, marketplace_id TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	h := &PluginHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db)}
	w := httptest.NewRecorder()
	h.HandleSyncMarketplaces(w, httptest.NewRequest("POST", "/sync", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("sync without catalog service must fail: %d", w.Code)
	}
	stub := &stubCatalogSync{}
	h.CatalogSync = stub
	w = httptest.NewRecorder()
	h.HandleSyncMarketplaces(w, httptest.NewRequest("POST", "/sync", nil))
	if w.Code != http.StatusOK || len(stub.synced) != 1 {
		t.Fatalf("sync: %d %s %v", w.Code, w.Body.String(), stub.synced)
	}
}
