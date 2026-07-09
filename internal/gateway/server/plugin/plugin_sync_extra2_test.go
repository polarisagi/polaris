package plugin

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/protocol"
)

func TestParsePluginEntry(t *testing.T) {
	_, err := parsePluginEntry("path", "repo", protocol.Marketplace{})
	if err == nil {
		t.Errorf("expected error")
	}
}

func TestParseMCPEntry(t *testing.T) {
	_, err := parseMCPEntry("path", "repo", protocol.Marketplace{})
	if err == nil {
		t.Errorf("expected error")
	}
}

func TestParseBundleManifest(t *testing.T) {
	parseBundleManifest("path", "plugin.json", "repo", protocol.Marketplace{})
}

func TestDiscoverMarketplaceEntries(t *testing.T) {
	discoverMarketplaceEntries("repo", protocol.Marketplace{})
}

func TestPullOrClone(t *testing.T) {
	downloader.Configure("off", nil)
	pullOrClone("http://127.0.0.1:0/bad", filepath.Join(t.TempDir(), "mpDir"))
}

func TestSyncMarketplace(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = &PluginHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db)}
}

func TestInsertMarketplaceEntries(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = &PluginHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db)}
}

func TestHandleSyncMarketplaces(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &PluginHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db)}
	w := httptest.NewRecorder()
	h.HandleSyncMarketplaces(w, httptest.NewRequest("POST", "/sync", nil))
}
